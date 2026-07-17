package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
	"github.com/google/uuid"
)

const (
	defaultKimiBaseURL   = "https://api.moonshot.cn/v1"
	defaultKimiAPIKeyEnv = "MOONSHOT_API_KEY"
)

var defaultKimiOpenAIModels = []config.ModelConfig{
	{Name: "kimi-k3", Model: "kimi-k3"},
	{Name: "kimi-k2.7-code-highspeed", Model: "kimi-k2.7-code-highspeed"},
	{Name: "kimi-k2.6", Model: "kimi-k2.6"},
}

var defaultKimiCodingModels = []config.ModelConfig{
	{Name: "kimi-k3", Model: "k3"},
	{Name: "kimi-for-coding", Model: "kimi-for-coding"},
	{Name: "kimi-for-coding-highspeed", Model: "kimi-for-coding-highspeed"},
}

// KimiExecutor connects the proxy's internal Chat Completions contract to the
// OpenAI-compatible Kimi API. It also implements AnthropicExecutor so Claude
// Code can use the same models through /v1/messages.
type KimiExecutor struct {
	mu         sync.RWMutex
	baseURL    string
	apiKeyEnv  string
	apiFormat  string
	models     []config.ModelConfig
	httpClient *http.Client
}

func NewKimiExecutor(cfg config.KimiConfig) *KimiExecutor {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultKimiBaseURL
	}
	apiKeyEnv := strings.TrimSpace(cfg.APIKeyEnv)
	if apiKeyEnv == "" {
		apiKeyEnv = defaultKimiAPIKeyEnv
	}
	apiFormat := strings.ToLower(strings.TrimSpace(cfg.APIFormat))
	if apiFormat != "anthropic" {
		apiFormat = "openai"
	}
	models := cfg.Models
	if len(models) == 0 {
		defaults := defaultKimiOpenAIModels
		if apiFormat == "anthropic" {
			defaults = defaultKimiCodingModels
		}
		models = append([]config.ModelConfig(nil), defaults...)
	}
	return &KimiExecutor{
		baseURL:    baseURL,
		apiKeyEnv:  apiKeyEnv,
		apiFormat:  apiFormat,
		models:     models,
		httpClient: http.DefaultClient,
	}
}

func (e *KimiExecutor) Models() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	models := make([]string, 0, len(e.models))
	for _, model := range e.models {
		models = append(models, model.Name)
	}
	return models
}

func (e *KimiExecutor) SetModels(models []config.ModelConfig) {
	e.mu.Lock()
	e.models = append([]config.ModelConfig(nil), models...)
	e.mu.Unlock()
}

func (e *KimiExecutor) Configured() bool {
	return strings.TrimSpace(os.Getenv(e.APIKeyEnv())) != ""
}

func (e *KimiExecutor) BaseURL() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.baseURL
}

func (e *KimiExecutor) APIKeyEnv() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.apiKeyEnv
}

func (e *KimiExecutor) APIFormat() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.apiFormat
}

func (e *KimiExecutor) resolveModel(alias string) string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, model := range e.models {
		if model.Name == alias {
			return model.Model
		}
	}
	return alias
}

func (e *KimiExecutor) apiKey() (string, error) {
	envName := e.APIKeyEnv()
	key := strings.TrimSpace(os.Getenv(envName))
	if key == "" {
		return "", fmt.Errorf("kimi API key environment variable %s is not set", envName)
	}
	return key, nil
}

func (e *KimiExecutor) openChat(ctx context.Context, req *types.ChatCompletionRequest, stream bool) (*http.Response, error) {
	key, err := e.apiKey()
	if err != nil {
		return nil, err
	}

	upstreamReq := *req
	upstreamReq.Model = e.resolveModel(req.Model)
	upstreamReq.Stream = stream
	// Kimi K3 currently accepts only the top-level "max" reasoning effort.
	// Codex commonly sends low/medium/high/xhigh, so normalize those values at
	// the provider boundary instead of exposing an avoidable upstream 400.
	if upstreamReq.Model == "kimi-k3" && upstreamReq.ReasoningEffort != "" {
		upstreamReq.ReasoningEffort = "max"
	}
	payload, err := json.Marshal(&upstreamReq)
	if err != nil {
		return nil, fmt.Errorf("marshal kimi request: %w", err)
	}
	if stream {
		var withOptions map[string]interface{}
		if json.Unmarshal(payload, &withOptions) == nil {
			withOptions["stream_options"] = map[string]bool{"include_usage": true}
			payload, _ = json.Marshal(withOptions)
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL()+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+key)
	httpReq.Header.Set("Content-Type", "application/json")
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}

	resp, err := e.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("kimi request: %w", err)
	}
	return resp, nil
}

func (e *KimiExecutor) Execute(ctx context.Context, req *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	if e.APIFormat() == "anthropic" {
		return e.executeAnthropicChat(ctx, req)
	}
	resp, err := e.openChat(ctx, req, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read kimi response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{Backend: "kimi", Status: resp.StatusCode, Body: string(body)}
	}

	var completion types.ChatCompletionResponse
	if err := json.Unmarshal(body, &completion); err != nil {
		return nil, fmt.Errorf("parse kimi response: %w", err)
	}
	completion.Model = req.Model
	return &completion, nil
}

func (e *KimiExecutor) ExecuteStream(ctx context.Context, req *types.ChatCompletionRequest, w io.Writer) (*types.Usage, error) {
	if e.APIFormat() == "anthropic" {
		return e.executeAnthropicChatStream(ctx, req, w)
	}
	resp, err := e.openChat(ctx, req, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, &HTTPError{Backend: "kimi", Status: resp.StatusCode, Body: string(body)}
	}

	flusher, canFlush := w.(interface{ Flush() })
	var usage types.Usage
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if data, ok := sseData(line); ok && data != "[DONE]" {
			var chunk types.ChatCompletionChunk
			if json.Unmarshal([]byte(data), &chunk) == nil && chunk.Usage != nil {
				usage = *chunk.Usage
			}
		}
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return &usage, err
		}
		if canFlush && line == "" {
			flusher.Flush()
		}
	}
	if err := scanner.Err(); err != nil {
		return &usage, err
	}
	return &usage, nil
}

func (e *KimiExecutor) anthropicEndpoint() string {
	baseURL := e.BaseURL()
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + "/messages"
	}
	return baseURL + "/v1/messages"
}

func (e *KimiExecutor) openAnthropic(ctx context.Context, body []byte, clientHeaders http.Header, stream bool) (*http.Response, error) {
	key, err := e.apiKey()
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.anthropicEndpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("x-api-key", key)
	httpReq.Header.Set("Content-Type", "application/json")
	version := "2023-06-01"
	if clientHeaders != nil && clientHeaders.Get("anthropic-version") != "" {
		version = clientHeaders.Get("anthropic-version")
	}
	httpReq.Header.Set("anthropic-version", version)
	if clientHeaders != nil && clientHeaders.Get("anthropic-beta") != "" {
		httpReq.Header.Set("anthropic-beta", clientHeaders.Get("anthropic-beta"))
	}
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	resp, err := e.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("kimi anthropic request: %w", err)
	}
	return resp, nil
}

func (e *KimiExecutor) executeAnthropicChat(ctx context.Context, req *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	upstreamModel := e.resolveModel(req.Model)
	anthropicReq := ToAnthropicRequest(req, upstreamModel)
	anthropicReq.Stream = false
	anthropicReq.AnthropicVersion = ""
	body, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("marshal kimi anthropic request: %w", err)
	}
	resp, err := e.openAnthropic(ctx, body, nil, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read kimi anthropic response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{Backend: "kimi", Status: resp.StatusCode, Body: string(respBody)}
	}
	var anthropicResp types.AnthropicResponse
	if err := json.Unmarshal(respBody, &anthropicResp); err != nil {
		return nil, fmt.Errorf("parse kimi anthropic response: %w", err)
	}
	return FromAnthropicResponse(&anthropicResp, req.Model), nil
}

func (e *KimiExecutor) executeAnthropicChatStream(ctx context.Context, req *types.ChatCompletionRequest, w io.Writer) (*types.Usage, error) {
	upstreamModel := e.resolveModel(req.Model)
	anthropicReq := ToAnthropicRequest(req, upstreamModel)
	anthropicReq.Stream = true
	anthropicReq.AnthropicVersion = ""
	body, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("marshal kimi anthropic request: %w", err)
	}
	resp, err := e.openAnthropic(ctx, body, nil, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, &HTTPError{Backend: "kimi", Status: resp.StatusCode, Body: string(respBody)}
	}

	chunkID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:24])
	created := time.Now().Unix()
	writeSSEChunk(w, types.ChatCompletionChunk{
		ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
		Choices: []types.ChatCompletionChoice{{Index: 0, Delta: &types.ChatResult{Role: "assistant"}}},
	})

	var usage types.Usage
	var hasToolCalls bool
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data, ok := sseData(scanner.Text())
		if !ok || data == "[DONE]" {
			continue
		}
		var event types.AnthropicStreamEvent
		if json.Unmarshal([]byte(data), &event) != nil {
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
				writeSSEChunk(w, types.ChatCompletionChunk{
					ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
					Choices: []types.ChatCompletionChoice{{
						Index: 0,
						Delta: &types.ChatResult{ToolCalls: []types.ToolCall{{
							Index: event.Index,
							ID:    event.ContentBlock.ID,
							Type:  "function",
							Function: types.ToolCallFunction{
								Name: event.ContentBlock.Name,
							},
						}}},
					}},
				})
			}
		case "content_block_delta":
			var delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
				JSON string `json:"partial_json"`
			}
			if json.Unmarshal(event.Delta, &delta) != nil {
				continue
			}
			switch delta.Type {
			case "text_delta":
				writeSSEChunk(w, types.ChatCompletionChunk{
					ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
					Choices: []types.ChatCompletionChoice{{Index: 0, Delta: &types.ChatResult{Content: delta.Text}}},
				})
			case "input_json_delta":
				writeSSEChunk(w, types.ChatCompletionChunk{
					ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
					Choices: []types.ChatCompletionChoice{{
						Index: 0,
						Delta: &types.ChatResult{ToolCalls: []types.ToolCall{{
							Index: event.Index,
							Function: types.ToolCallFunction{
								Arguments: delta.JSON,
							},
						}}},
					}},
				})
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
				var delta struct {
					StopReason string `json:"stop_reason"`
				}
				if json.Unmarshal(event.Delta, &delta) == nil && delta.StopReason != "" {
					finishReason = mapStopReason(delta.StopReason)
				}
			}
			writeSSEChunk(w, types.ChatCompletionChunk{
				ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []types.ChatCompletionChoice{{Index: 0, Delta: &types.ChatResult{}, FinishReason: &finishReason}},
				Usage:   &usage,
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return &usage, err
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	return &usage, nil
}

type kimiAnthropicRequest struct {
	Model         string                 `json:"model"`
	System        json.RawMessage        `json:"system,omitempty"`
	Messages      []kimiAnthropicMessage `json:"messages"`
	MaxTokens     int                    `json:"max_tokens"`
	Stream        bool                   `json:"stream,omitempty"`
	Temperature   *float64               `json:"temperature,omitempty"`
	TopP          *float64               `json:"top_p,omitempty"`
	StopSequences []string               `json:"stop_sequences,omitempty"`
	Tools         []types.AnthropicTool  `json:"tools,omitempty"`
	ToolChoice    json.RawMessage        `json:"tool_choice,omitempty"`
	Thinking      json.RawMessage        `json:"thinking,omitempty"`
}

type kimiAnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type kimiAnthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type,omitempty"`
		Data      string `json:"data,omitempty"`
		URL       string `json:"url,omitempty"`
	} `json:"source,omitempty"`
}

func anthropicToChatRequest(body []byte) (*types.ChatCompletionRequest, error) {
	var in kimiAnthropicRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("parse Anthropic request: %w", err)
	}
	if strings.TrimSpace(in.Model) == "" {
		return nil, fmt.Errorf("model is required")
	}

	out := &types.ChatCompletionRequest{
		Model:       in.Model,
		Stream:      in.Stream,
		MaxTokens:   in.MaxTokens,
		Temperature: in.Temperature,
		TopP:        in.TopP,
	}
	if len(in.StopSequences) > 0 {
		out.Stop, _ = json.Marshal(in.StopSequences)
	}
	if system := anthropicText(in.System); system != "" {
		content, _ := json.Marshal(system)
		out.Messages = append(out.Messages, types.ChatMessage{Role: "system", Content: content})
	}

	for _, message := range in.Messages {
		converted, err := convertAnthropicMessage(message)
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, converted...)
	}
	for _, tool := range in.Tools {
		out.Tools = append(out.Tools, types.Tool{
			Type: "function",
			Function: types.ToolFunction{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.InputSchema,
			},
		})
	}
	out.ToolChoice = convertAnthropicToolChoice(in.ToolChoice)
	if len(in.Thinking) > 0 && !bytes.Equal(bytes.TrimSpace(in.Thinking), []byte("null")) {
		var thinking struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(in.Thinking, &thinking) == nil && thinking.Type != "disabled" {
			out.ReasoningEffort = "max"
		}
	}
	return out, nil
}

func anthropicText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []kimiAnthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func convertAnthropicMessage(message kimiAnthropicMessage) ([]types.ChatMessage, error) {
	var plain string
	if json.Unmarshal(message.Content, &plain) == nil {
		content, _ := json.Marshal(plain)
		return []types.ChatMessage{{Role: message.Role, Content: content}}, nil
	}

	var blocks []kimiAnthropicBlock
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return nil, fmt.Errorf("parse %s message content: %w", message.Role, err)
	}
	if message.Role == "assistant" {
		return convertAnthropicAssistant(blocks), nil
	}
	return convertAnthropicUser(blocks), nil
}

func convertAnthropicAssistant(blocks []kimiAnthropicBlock) []types.ChatMessage {
	var textParts []string
	var toolCalls []types.ToolCall
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				textParts = append(textParts, block.Text)
			}
		case "tool_use":
			arguments := string(block.Input)
			if arguments == "" || arguments == "null" {
				arguments = "{}"
			}
			toolCalls = append(toolCalls, types.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: types.ToolCallFunction{
					Name:      block.Name,
					Arguments: arguments,
				},
			})
		}
	}
	content, _ := json.Marshal(strings.Join(textParts, "\n"))
	return []types.ChatMessage{{Role: "assistant", Content: content, ToolCalls: toolCalls}}
}

func convertAnthropicUser(blocks []kimiAnthropicBlock) []types.ChatMessage {
	var messages []types.ChatMessage
	var contentBlocks []map[string]interface{}
	flushContent := func() {
		if len(contentBlocks) == 0 {
			return
		}
		content, _ := json.Marshal(contentBlocks)
		messages = append(messages, types.ChatMessage{Role: "user", Content: content})
		contentBlocks = nil
	}

	for _, block := range blocks {
		switch block.Type {
		case "text":
			contentBlocks = append(contentBlocks, map[string]interface{}{"type": "text", "text": block.Text})
		case "image":
			if block.Source == nil {
				continue
			}
			url := block.Source.URL
			if block.Source.Type == "base64" && block.Source.Data != "" {
				url = "data:" + block.Source.MediaType + ";base64," + block.Source.Data
			}
			if url != "" {
				contentBlocks = append(contentBlocks, map[string]interface{}{
					"type":      "image_url",
					"image_url": map[string]string{"url": url},
				})
			}
		case "tool_result":
			flushContent()
			text := anthropicText(block.Content)
			if text == "" && len(block.Content) > 0 {
				text = string(block.Content)
			}
			content, _ := json.Marshal(text)
			messages = append(messages, types.ChatMessage{
				Role:       "tool",
				Content:    content,
				ToolCallID: block.ToolUseID,
			})
		}
	}
	flushContent()
	if len(messages) == 0 {
		content, _ := json.Marshal("")
		messages = append(messages, types.ChatMessage{Role: "user", Content: content})
	}
	return messages
}

func convertAnthropicToolChoice(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name,omitempty"`
	}
	if json.Unmarshal(raw, &choice) != nil {
		return nil
	}
	var value interface{}
	switch choice.Type {
	case "auto":
		value = "auto"
	case "any":
		value = "required"
	case "none":
		value = "none"
	case "tool":
		value = map[string]interface{}{
			"type":     "function",
			"function": map[string]string{"name": choice.Name},
		}
	default:
		return nil
	}
	converted, _ := json.Marshal(value)
	return converted
}

func (e *KimiExecutor) rewriteAnthropicModel(body []byte) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse Anthropic request: %w", err)
	}
	var model string
	if err := json.Unmarshal(payload["model"], &model); err != nil || strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("model is required")
	}
	payload["model"], _ = json.Marshal(e.resolveModel(model))
	return json.Marshal(payload)
}

func (e *KimiExecutor) executeAnthropicPassthroughRaw(ctx context.Context, body []byte, clientHeaders http.Header) ([]byte, int, error) {
	rewritten, err := e.rewriteAnthropicModel(body)
	if err != nil {
		return anthropicErrorJSON("invalid_request_error", err.Error()), http.StatusBadRequest, nil
	}
	resp, err := e.openAnthropic(ctx, rewritten, clientHeaders, false)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("read kimi anthropic response: %w", err)
	}
	return respBody, resp.StatusCode, nil
}

func (e *KimiExecutor) openAnthropicPassthrough(ctx context.Context, body []byte, clientHeaders http.Header) (io.ReadCloser, int, error) {
	rewritten, err := e.rewriteAnthropicModel(body)
	if err != nil {
		return io.NopCloser(bytes.NewReader(anthropicErrorJSON("invalid_request_error", err.Error()))), http.StatusBadRequest, nil
	}
	resp, err := e.openAnthropic(ctx, rewritten, clientHeaders, true)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.StatusCode, nil
}

func (e *KimiExecutor) ExecuteAnthropicRaw(ctx context.Context, body []byte, clientHeaders http.Header) ([]byte, int, error) {
	if e.APIFormat() == "anthropic" {
		return e.executeAnthropicPassthroughRaw(ctx, body, clientHeaders)
	}
	req, err := anthropicToChatRequest(body)
	if err != nil {
		return anthropicErrorJSON("invalid_request_error", err.Error()), http.StatusBadRequest, nil
	}
	req.Stream = false
	resp, err := e.Execute(ctx, req)
	if err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) {
			return kimiErrorToAnthropic([]byte(httpErr.Body), httpErr.Status), httpErr.Status, nil
		}
		return nil, 0, err
	}
	converted := chatToAnthropicResponse(resp, req.Model)
	data, err := json.Marshal(converted)
	if err != nil {
		return nil, 0, err
	}
	return data, http.StatusOK, nil
}

func chatToAnthropicResponse(resp *types.ChatCompletionResponse, model string) *types.AnthropicResponse {
	result := &types.AnthropicResponse{
		ID:      "msg_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		Type:    "message",
		Role:    "assistant",
		Content: []types.AnthropicContentBlock{},
		Model:   model,
	}
	if resp.Usage != nil {
		result.Usage.InputTokens = resp.Usage.PromptTokens
		result.Usage.OutputTokens = resp.Usage.CompletionTokens
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message == nil {
		result.StopReason = "end_turn"
		return result
	}
	choice := resp.Choices[0]
	if choice.Message.Content != "" {
		result.Content = append(result.Content, types.AnthropicContentBlock{
			Type: "text",
			Text: choice.Message.Content,
		})
	}
	for _, call := range choice.Message.ToolCalls {
		input := json.RawMessage(call.Function.Arguments)
		if len(input) == 0 || !json.Valid(input) {
			input = json.RawMessage(`{}`)
		}
		result.Content = append(result.Content, types.AnthropicContentBlock{
			Type:  "tool_use",
			ID:    call.ID,
			Name:  call.Function.Name,
			Input: input,
		})
	}
	finishReason := "stop"
	if choice.FinishReason != nil {
		finishReason = *choice.FinishReason
	}
	result.StopReason = openAIToAnthropicStopReason(finishReason)
	return result
}

func openAIToAnthropicStopReason(reason string) string {
	switch reason {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

func anthropicErrorJSON(errType, message string) []byte {
	data, _ := json.Marshal(map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
	return data
}

func kimiErrorToAnthropic(body []byte, status int) []byte {
	var upstream struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	message := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &upstream) == nil && upstream.Error.Message != "" {
		message = upstream.Error.Message
	}
	if message == "" {
		message = http.StatusText(status)
	}
	return anthropicErrorJSON("api_error", message)
}

func (e *KimiExecutor) OpenAnthropicStream(ctx context.Context, body []byte, clientHeaders http.Header) (io.ReadCloser, int, error) {
	if e.APIFormat() == "anthropic" {
		return e.openAnthropicPassthrough(ctx, body, clientHeaders)
	}
	req, err := anthropicToChatRequest(body)
	if err != nil {
		return io.NopCloser(bytes.NewReader(anthropicErrorJSON("invalid_request_error", err.Error()))), http.StatusBadRequest, nil
	}
	req.Stream = true
	resp, err := e.openChat(ctx, req, true)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return io.NopCloser(bytes.NewReader(kimiErrorToAnthropic(body, resp.StatusCode))), resp.StatusCode, nil
	}

	reader, writer := io.Pipe()
	go func() {
		defer resp.Body.Close()
		err := translateChatStreamToAnthropic(resp.Body, writer, req.Model)
		writer.CloseWithError(err)
	}()
	return reader, http.StatusOK, nil
}

type anthropicToolStreamState struct {
	blockIndex int
	callID     string
	name       string
	started    bool
	closed     bool
}

func translateChatStreamToAnthropic(src io.Reader, dst io.Writer, model string) error {
	messageID := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	writeEvent := func(eventType string, payload interface{}) error {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(dst, "event: %s\ndata: %s\n\n", eventType, data)
		return err
	}

	if err := writeEvent("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id": messageID, "type": "message", "role": "assistant",
			"content": []interface{}{}, "model": model,
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	}); err != nil {
		return err
	}

	var usage types.Usage
	finishReason := "stop"
	textStarted := false
	textClosed := false
	textBlockIndex := -1
	nextBlockIndex := 0
	toolStates := make(map[int]*anthropicToolStreamState)
	toolOrder := make([]int, 0)
	lastToolIndex := 0

	startText := func() error {
		if textStarted {
			return nil
		}
		textStarted = true
		textBlockIndex = nextBlockIndex
		nextBlockIndex++
		return writeEvent("content_block_start", map[string]interface{}{
			"type": "content_block_start", "index": textBlockIndex,
			"content_block": map[string]interface{}{"type": "text", "text": ""},
		})
	}

	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data, ok := sseData(scanner.Text())
		if !ok {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var chunk types.ChatCompletionChunk
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
			}
			if choice.Delta == nil {
				continue
			}
			if choice.Delta.Content != "" {
				if err := startText(); err != nil {
					return err
				}
				if err := writeEvent("content_block_delta", map[string]interface{}{
					"type": "content_block_delta", "index": textBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": choice.Delta.Content},
				}); err != nil {
					return err
				}
			}
			for _, call := range choice.Delta.ToolCalls {
				toolIndex := call.Index
				if call.ID == "" && toolIndex == 0 {
					toolIndex = lastToolIndex
				}
				state := toolStates[toolIndex]
				if state == nil {
					state = &anthropicToolStreamState{blockIndex: nextBlockIndex, callID: call.ID, name: call.Function.Name}
					nextBlockIndex++
					toolStates[toolIndex] = state
					toolOrder = append(toolOrder, toolIndex)
				}
				lastToolIndex = toolIndex
				if call.ID != "" {
					state.callID = call.ID
				}
				if call.Function.Name != "" {
					state.name = call.Function.Name
				}
				if !state.started && (state.callID != "" || state.name != "") {
					state.started = true
					if err := writeEvent("content_block_start", map[string]interface{}{
						"type": "content_block_start", "index": state.blockIndex,
						"content_block": map[string]interface{}{
							"type": "tool_use", "id": state.callID, "name": state.name, "input": map[string]interface{}{},
						},
					}); err != nil {
						return err
					}
				}
				if call.Function.Arguments != "" {
					if !state.started {
						state.started = true
						if err := writeEvent("content_block_start", map[string]interface{}{
							"type": "content_block_start", "index": state.blockIndex,
							"content_block": map[string]interface{}{
								"type": "tool_use", "id": state.callID, "name": state.name, "input": map[string]interface{}{},
							},
						}); err != nil {
							return err
						}
					}
					if err := writeEvent("content_block_delta", map[string]interface{}{
						"type": "content_block_delta", "index": state.blockIndex,
						"delta": map[string]string{"type": "input_json_delta", "partial_json": call.Function.Arguments},
					}); err != nil {
						return err
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	if !textStarted && len(toolStates) == 0 {
		if err := startText(); err != nil {
			return err
		}
	}
	if textStarted && !textClosed {
		if err := writeEvent("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": textBlockIndex}); err != nil {
			return err
		}
		textClosed = true
	}
	for _, index := range toolOrder {
		state := toolStates[index]
		if state.started && !state.closed {
			if err := writeEvent("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": state.blockIndex}); err != nil {
				return err
			}
			state.closed = true
		}
	}
	if err := writeEvent("message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   openAIToAnthropicStopReason(finishReason),
			"stop_sequence": nil,
		},
		"usage": map[string]int{"output_tokens": usage.CompletionTokens},
	}); err != nil {
		return err
	}
	return writeEvent("message_stop", map[string]string{"type": "message_stop"})
}

func sseData(line string) (string, bool) {
	if strings.HasPrefix(line, "data: ") {
		return strings.TrimPrefix(line, "data: "), true
	}
	if strings.HasPrefix(line, "data:") {
		return strings.TrimSpace(strings.TrimPrefix(line, "data:")), true
	}
	return "", false
}
