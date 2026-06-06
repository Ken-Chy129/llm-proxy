package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/user/cli-proxy/internal/config"
	"github.com/user/cli-proxy/internal/types"
	"golang.org/x/oauth2/google"
)

type VertexExecutor struct {
	cfg    config.VertexConfig
	models []string
}

func NewVertexExecutor(cfg config.VertexConfig) *VertexExecutor {
	models := make([]string, len(cfg.Models))
	for i, m := range cfg.Models {
		models[i] = m.Name
	}
	return &VertexExecutor{cfg: cfg, models: models}
}

func (e *VertexExecutor) Models() []string { return e.models }

func (e *VertexExecutor) resolveModel(name string) string {
	for _, m := range e.cfg.Models {
		if m.Name == name {
			return m.Model
		}
	}
	return name
}

func (e *VertexExecutor) getToken(ctx context.Context) (string, error) {
	creds, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return "", fmt.Errorf("gcp credentials: %w", err)
	}
	tok, err := creds.TokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("gcp token: %w", err)
	}
	return tok.AccessToken, nil
}

func (e *VertexExecutor) buildURL(model string, stream bool) string {
	action := "rawPredict"
	if stream {
		action = "streamRawPredict"
	}
	return fmt.Sprintf(
		"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:%s",
		e.cfg.Region, e.cfg.ProjectID, e.cfg.Region, model, action,
	)
}

func (e *VertexExecutor) Execute(ctx context.Context, req *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	vertexModel := e.resolveModel(req.Model)
	ar := ToAnthropicRequest(req, vertexModel)
	ar.Stream = false
	ar.Model = "" // Vertex rawPredict: model is in URL, not body

	token, err := e.getToken(ctx)
	if err != nil {
		return nil, err
	}

	body, _ := json.Marshal(ar)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.buildURL(vertexModel, false), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("vertex request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vertex error %d: %s", resp.StatusCode, string(respBody))
	}

	var anthropicResp types.AnthropicResponse
	if err := json.Unmarshal(respBody, &anthropicResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return FromAnthropicResponse(&anthropicResp, req.Model), nil
}

func (e *VertexExecutor) ExecuteStream(ctx context.Context, req *types.ChatCompletionRequest, w io.Writer) error {
	vertexModel := e.resolveModel(req.Model)
	ar := ToAnthropicRequest(req, vertexModel)
	ar.Stream = true
	ar.Model = "" // Vertex rawPredict: model is in URL, not body

	token, err := e.getToken(ctx)
	if err != nil {
		return err
	}

	body, _ := json.Marshal(ar)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.buildURL(vertexModel, true), bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("vertex stream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("vertex error %d: %s", resp.StatusCode, string(respBody))
	}

	chunkID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:24])
	created := time.Now().Unix()

	// Send initial role chunk
	writeSSEChunk(w, types.ChatCompletionChunk{
		ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
		Choices: []types.ChatCompletionChoice{
			{Index: 0, Delta: &types.ChatResult{Role: "assistant"}},
		},
	})

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	var hasToolCalls bool
	var currentToolIndex int

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var event types.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		switch event.Type {
		case "content_block_start":
			if event.ContentBlock != nil && event.ContentBlock.Type == "tool_use" {
				hasToolCalls = true
				tc := types.ToolCall{
					ID:   event.ContentBlock.ID,
					Type: "function",
					Function: types.ToolCallFunction{
						Name: event.ContentBlock.Name,
					},
				}
				writeSSEChunk(w, types.ChatCompletionChunk{
					ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
					Choices: []types.ChatCompletionChoice{
						{Index: 0, Delta: &types.ChatResult{ToolCalls: []types.ToolCall{tc}}},
					},
				})
				currentToolIndex++
			}

		case "content_block_delta":
			if len(event.Delta) > 0 {
				var delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
					JSON string `json:"partial_json"`
				}
				json.Unmarshal(event.Delta, &delta)

				switch delta.Type {
				case "text_delta":
					writeSSEChunk(w, types.ChatCompletionChunk{
						ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
						Choices: []types.ChatCompletionChoice{
							{Index: 0, Delta: &types.ChatResult{Content: delta.Text}},
						},
					})
				case "input_json_delta":
					tc := types.ToolCall{
						Function: types.ToolCallFunction{Arguments: delta.JSON},
					}
					writeSSEChunk(w, types.ChatCompletionChunk{
						ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
						Choices: []types.ChatCompletionChoice{
							{Index: 0, Delta: &types.ChatResult{ToolCalls: []types.ToolCall{tc}}},
						},
					})
				}
			}

		case "message_delta":
			finishReason := "stop"
			if hasToolCalls {
				finishReason = "tool_calls"
			}
			if len(event.Delta) > 0 {
				var d struct {
					StopReason string `json:"stop_reason"`
				}
				json.Unmarshal(event.Delta, &d)
				if d.StopReason != "" {
					finishReason = mapStopReason(d.StopReason)
				}
			}
			writeSSEChunk(w, types.ChatCompletionChunk{
				ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []types.ChatCompletionChoice{
					{Index: 0, Delta: &types.ChatResult{}, FinishReason: &finishReason},
				},
			})
		}
	}

	fmt.Fprint(w, "data: [DONE]\n\n")
	return nil
}

func writeSSEChunk(w io.Writer, chunk types.ChatCompletionChunk) {
	data, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func (e *VertexExecutor) prepareAnthropicBody(body []byte) ([]byte, string, error) {
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, "", fmt.Errorf("parse body: %w", err)
	}

	var modelName string
	if raw, ok := parsed["model"]; ok {
		json.Unmarshal(raw, &modelName)
	}
	vertexModel := e.resolveModel(modelName)

	delete(parsed, "model")
	parsed["anthropic_version"] = json.RawMessage(`"vertex-2023-10-16"`)

	modified, err := json.Marshal(parsed)
	if err != nil {
		return nil, "", fmt.Errorf("marshal body: %w", err)
	}
	return modified, vertexModel, nil
}

func (e *VertexExecutor) ExecuteAnthropicRaw(ctx context.Context, body []byte, clientHeaders http.Header) ([]byte, int, error) {
	modified, vertexModel, err := e.prepareAnthropicBody(body)
	if err != nil {
		return nil, 0, err
	}

	token, err := e.getToken(ctx)
	if err != nil {
		return nil, 0, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		e.buildURL(vertexModel, false), bytes.NewReader(modified))
	if err != nil {
		return nil, 0, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("vertex request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("read response: %w", err)
	}
	return respBody, resp.StatusCode, nil
}

func (e *VertexExecutor) OpenAnthropicStream(ctx context.Context, body []byte, clientHeaders http.Header) (io.ReadCloser, int, error) {
	modified, vertexModel, err := e.prepareAnthropicBody(body)
	if err != nil {
		return nil, 0, err
	}

	token, err := e.getToken(ctx)
	if err != nil {
		return nil, 0, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		e.buildURL(vertexModel, true), bytes.NewReader(modified))
	if err != nil {
		return nil, 0, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("vertex stream request: %w", err)
	}
	return resp.Body, resp.StatusCode, nil
}
