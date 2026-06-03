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
	"github.com/user/cli-proxy/internal/auth"
	internaltls "github.com/user/cli-proxy/internal/tls"
	"github.com/user/cli-proxy/internal/types"
)

type ClaudeOAuthExecutor struct {
	oauth      *auth.ClaudeOAuth
	httpClient *http.Client
	models     []string
}

func NewClaudeOAuthExecutor(oauth *auth.ClaudeOAuth, models []string) *ClaudeOAuthExecutor {
	return &ClaudeOAuthExecutor{
		oauth:      oauth,
		httpClient: internaltls.NewAnthropicHTTPClient(),
		models:     models,
	}
}

func (e *ClaudeOAuthExecutor) Models() []string { return e.models }

func (e *ClaudeOAuthExecutor) Execute(ctx context.Context, req *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	ar := ToAnthropicRequest(req, req.Model)
	ar.Stream = false
	ar.AnthropicVersion = ""

	token, err := e.oauth.GetToken(ctx)
	if err != nil {
		return nil, err
	}

	body, _ := json.Marshal(ar)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyClaudeOAuthHeaders(httpReq, token)

	resp, err := e.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("claude oauth request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claude oauth error %d: %s", resp.StatusCode, string(respBody))
	}

	var anthropicResp types.AnthropicResponse
	if err := json.Unmarshal(respBody, &anthropicResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return FromAnthropicResponse(&anthropicResp, req.Model), nil
}

func (e *ClaudeOAuthExecutor) ExecuteStream(ctx context.Context, req *types.ChatCompletionRequest, w io.Writer) error {
	ar := ToAnthropicRequest(req, req.Model)
	ar.Stream = true
	ar.AnthropicVersion = ""

	token, err := e.oauth.GetToken(ctx)
	if err != nil {
		return err
	}

	body, _ := json.Marshal(ar)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", bytes.NewReader(body))
	if err != nil {
		return err
	}
	applyClaudeOAuthHeaders(httpReq, token)

	resp, err := e.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("claude oauth stream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("claude oauth error %d: %s", resp.StatusCode, string(respBody))
	}

	chunkID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:24])
	created := time.Now().Unix()

	writeSSEChunk(w, types.ChatCompletionChunk{
		ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
		Choices: []types.ChatCompletionChoice{
			{Index: 0, Delta: &types.ChatResult{Role: "assistant"}},
		},
	})

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	var hasToolCalls bool

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
					Function: types.ToolCallFunction{Name: event.ContentBlock.Name},
				}
				writeSSEChunk(w, types.ChatCompletionChunk{
					ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
					Choices: []types.ChatCompletionChoice{
						{Index: 0, Delta: &types.ChatResult{ToolCalls: []types.ToolCall{tc}}},
					},
				})
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

func applyClaudeOAuthHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")
	req.Header.Set("User-Agent", "Claude-Code/1.0")
}
