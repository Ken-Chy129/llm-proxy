package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/user/cli-proxy/internal/auth"
	internaltls "github.com/user/cli-proxy/internal/tls"
	"github.com/user/cli-proxy/internal/types"
)

type ClaudeOAuthExecutor struct {
	oauth          *auth.ClaudeOAuth
	httpClient     *http.Client
	models         []string
	lastBetaFlags  string
	betaMu         sync.RWMutex
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
	ar.Thinking = &types.ThinkingConfig{Type: "adaptive"}
	ar.MaxTokens = 64000

	token, err := e.oauth.GetToken(ctx)
	if err != nil {
		return nil, err
	}

	body, _ := json.Marshal(ar)
	body = injectClaudeCodeSystemBlocks(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	e.applyHeaders(httpReq, token)

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

func (e *ClaudeOAuthExecutor) ExecuteStream(ctx context.Context, req *types.ChatCompletionRequest, w io.Writer) (*types.Usage, error) {
	ar := ToAnthropicRequest(req, req.Model)
	ar.Stream = true
	ar.AnthropicVersion = ""
	ar.Thinking = &types.ThinkingConfig{Type: "adaptive"}
	ar.MaxTokens = 64000

	token, err := e.oauth.GetToken(ctx)
	if err != nil {
		return nil, err
	}

	body, _ := json.Marshal(ar)
	body = injectClaudeCodeSystemBlocks(body)
	log.Printf("[DEBUG-CHAT] URL=https://api.anthropic.com/v1/messages body=%s", string(body))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	e.applyHeaders(httpReq, token)
	log.Printf("[DEBUG-CHAT] headers: %v", httpReq.Header)

	resp, err := e.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("claude oauth stream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		log.Printf("[DEBUG-CHAT] error response headers: %v", resp.Header)
		return nil, fmt.Errorf("claude oauth error %d: %s", resp.StatusCode, string(respBody))
	}

	chunkID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:24])
	created := time.Now().Unix()

	writeSSEChunk(w, types.ChatCompletionChunk{
		ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
		Choices: []types.ChatCompletionChoice{
			{Index: 0, Delta: &types.ChatResult{Role: "assistant"}},
		},
	})

	var usage types.Usage
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
		case "message_start":
			if event.Message != nil {
				usage.PromptTokens = event.Message.Usage.InputTokens
			}

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
			if event.Usage != nil {
				usage.CompletionTokens = event.Usage.OutputTokens
				usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
			}
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
	return &usage, nil
}

const defaultClaudeBeta = "claude-code-20250219,context-1m-2025-08-07,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,mid-conversation-system-2026-04-07,advisor-tool-2026-03-01,effort-2025-11-24"

func (e *ClaudeOAuthExecutor) getClaudeBeta() string {
	e.betaMu.RLock()
	defer e.betaMu.RUnlock()
	if e.lastBetaFlags != "" {
		return e.lastBetaFlags
	}
	return defaultClaudeBeta
}

func (e *ClaudeOAuthExecutor) applyHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", e.getClaudeBeta())
}

func (e *ClaudeOAuthExecutor) ExecuteAnthropicRaw(ctx context.Context, body []byte, clientHeaders http.Header) ([]byte, int, error) {
	token, err := e.oauth.GetToken(ctx)
	if err != nil {
		return nil, 0, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	applyAnthropicPassthroughHeaders(httpReq, token, clientHeaders)

	resp, err := e.httpClient.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("claude oauth request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("read response: %w", err)
	}
	return respBody, resp.StatusCode, nil
}

func (e *ClaudeOAuthExecutor) OpenAnthropicStream(ctx context.Context, body []byte, clientHeaders http.Header) (io.ReadCloser, int, error) {
	token, err := e.oauth.GetToken(ctx)
	if err != nil {
		return nil, 0, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	applyAnthropicPassthroughHeaders(httpReq, token, clientHeaders)
	if beta := clientHeaders.Get("anthropic-beta"); beta != "" {
		e.betaMu.Lock()
		e.lastBetaFlags = beta
		e.betaMu.Unlock()
	}
	log.Printf("[DEBUG-PASSTHROUGH] headers: %v", httpReq.Header)
	var bodyPeek map[string]json.RawMessage
	json.Unmarshal(body, &bodyPeek)
	keys := make([]string, 0, len(bodyPeek))
	for k := range bodyPeek {
		keys = append(keys, k)
	}
	log.Printf("[DEBUG-PASSTHROUGH] body keys=%v model=%s max_tokens=%s thinking=%s", keys, bodyPeek["model"], bodyPeek["max_tokens"], bodyPeek["thinking"])

	resp, err := e.httpClient.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("claude oauth stream request: %w", err)
	}
	return resp.Body, resp.StatusCode, nil
}

func applyAnthropicPassthroughHeaders(req *http.Request, token string, clientHeaders http.Header) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if v := clientHeaders.Get("anthropic-version"); v != "" {
		req.Header.Set("anthropic-version", v)
	} else {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	if v := clientHeaders.Get("anthropic-beta"); v != "" {
		req.Header.Set("anthropic-beta", v)
	}
}
