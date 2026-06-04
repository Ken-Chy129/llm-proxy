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

const (
	codexBaseURL   = "https://chatgpt.com/backend-api"
	codexUserAgent = "codex-tui/0.135.0 (Mac OS 26.5.0; arm64)"
)

// Codex Responses API types

type codexRequest struct {
	Model              string            `json:"model"`
	Instructions       string            `json:"instructions,omitempty"`
	Input              []codexInputItem  `json:"input"`
	Stream             bool              `json:"stream"`
	Store              bool              `json:"store"`
	Reasoning          *codexReasoning   `json:"reasoning,omitempty"`
	Tools              []json.RawMessage `json:"tools,omitempty"`
	ToolChoice         interface{}       `json:"tool_choice,omitempty"`
	ParallelToolCalls  bool              `json:"parallel_tool_calls,omitempty"`
	ServiceTier        string            `json:"service_tier,omitempty"`
}

type codexInputItem struct {
	Role      string      `json:"role,omitempty"`
	Content   interface{} `json:"content,omitempty"`
	Type      string      `json:"type,omitempty"`
	CallID    string      `json:"call_id,omitempty"`
	Name      string      `json:"name,omitempty"`
	Arguments string      `json:"arguments,omitempty"`
	Output    string      `json:"output,omitempty"`
}

type codexReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type CodexExecutor struct {
	oauth  *auth.CodexOAuth
	models []string
}

func NewCodexExecutor(oauth *auth.CodexOAuth, models []string) *CodexExecutor {
	return &CodexExecutor{oauth: oauth, models: models}
}

func (e *CodexExecutor) Models() []string { return e.models }

func (e *CodexExecutor) toCodexRequest(req *types.ChatCompletionRequest) *codexRequest {
	cr := &codexRequest{
		Model:  req.Model,
		Stream: true,
		Store:  false,
	}

	var systemParts []string
	for _, msg := range req.Messages {
		if msg.Role == "system" || msg.Role == "developer" {
			systemParts = append(systemParts, extractText(msg.Content))
		}
	}
	if len(systemParts) > 0 {
		cr.Instructions = strings.Join(systemParts, "\n\n")
	}

	for _, msg := range req.Messages {
		switch msg.Role {
		case "system", "developer":
			continue
		case "user":
			cr.Input = append(cr.Input, codexInputItem{
				Role:    "user",
				Content: extractText(msg.Content),
			})
		case "assistant":
			text := extractText(msg.Content)
			if text != "" || len(msg.ToolCalls) == 0 {
				cr.Input = append(cr.Input, codexInputItem{
					Role:    "assistant",
					Content: text,
				})
			}
			for _, tc := range msg.ToolCalls {
				cr.Input = append(cr.Input, codexInputItem{
					Type:      "function_call",
					CallID:    tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
		case "tool":
			cr.Input = append(cr.Input, codexInputItem{
				Type:   "function_call_output",
				CallID: msg.ToolCallID,
				Output: extractText(msg.Content),
			})
		}
	}

	if len(cr.Input) == 0 {
		cr.Input = append(cr.Input, codexInputItem{Role: "user", Content: ""})
	}

	if req.ReasoningEffort != "" {
		cr.Reasoning = &codexReasoning{Effort: req.ReasoningEffort, Summary: "auto"}
	}

	// Convert OpenAI tools to Codex format (they're compatible)
	for _, tool := range req.Tools {
		raw, _ := json.Marshal(tool)
		cr.Tools = append(cr.Tools, raw)
	}
	if len(req.ToolChoice) > 0 {
		var tc interface{}
		json.Unmarshal(req.ToolChoice, &tc)
		cr.ToolChoice = tc
	}
	if len(cr.Tools) > 0 {
		cr.ParallelToolCalls = true
	}

	return cr
}

func (e *CodexExecutor) Execute(ctx context.Context, req *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	// Codex always streams; collect the stream for non-streaming response
	var buf bytes.Buffer
	if err := e.doStream(ctx, req, &buf); err != nil {
		return nil, err
	}

	// Parse collected SSE events
	result := &types.ChatResult{Role: "assistant"}
	var toolCalls []types.ToolCall
	var usage types.Usage
	toolCallMap := make(map[string]*types.ToolCall)

	scanner := bufio.NewScanner(&buf)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var event map[string]interface{}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		eventType, _ := event["type"].(string)
		switch eventType {
		case "response.output_text.delta":
			if delta, ok := event["delta"].(string); ok {
				result.Content += delta
			}
		case "response.function_call_arguments.done":
			callID, _ := event["call_id"].(string)
			name, _ := event["name"].(string)
			args, _ := event["arguments"].(string)
			tc := types.ToolCall{
				ID:   callID,
				Type: "function",
				Function: types.ToolCallFunction{Name: name, Arguments: args},
			}
			toolCallMap[callID] = &tc
		case "response.completed":
			if resp, ok := event["response"].(map[string]interface{}); ok {
				if u, ok := resp["usage"].(map[string]interface{}); ok {
					if v, ok := u["input_tokens"].(float64); ok {
						usage.PromptTokens = int(v)
					}
					if v, ok := u["output_tokens"].(float64); ok {
						usage.CompletionTokens = int(v)
					}
					usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
				}
			}
		}
	}

	for _, tc := range toolCallMap {
		toolCalls = append(toolCalls, *tc)
	}
	if len(toolCalls) > 0 {
		result.ToolCalls = toolCalls
	}

	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	return &types.ChatCompletionResponse{
		ID:      fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:24]),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []types.ChatCompletionChoice{
			{Index: 0, Message: result, FinishReason: &finishReason},
		},
		Usage: &usage,
	}, nil
}

func (e *CodexExecutor) ExecuteStream(ctx context.Context, req *types.ChatCompletionRequest, w io.Writer) error {
	return e.doStream(ctx, req, &sseTranslator{w: w, model: req.Model})
}

// ExecuteRawStream sends a raw JSON body to Codex /responses and writes SSE output to w.
func (e *CodexExecutor) ExecuteRawStream(ctx context.Context, rawBody []byte, w io.Writer) error {
	tokenData := e.oauth.GetTokenData(ctx)
	if tokenData == nil {
		return fmt.Errorf("codex not authenticated")
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, codexBaseURL+"/codex/responses", bytes.NewReader(rawBody))
	if err != nil {
		return err
	}

	httpReq.Header.Set("Authorization", "Bearer "+tokenData.AccessToken)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("User-Agent", codexUserAgent)
	httpReq.Header.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
	httpReq.Header.Set("x-openai-internal-codex-residency", "us")
	httpReq.Header.Set("x-codex-installation-id", uuid.New().String())
	httpReq.Header.Set("x-client-request-id", uuid.New().String())

	resp, err := internaltls.NewAnthropicHTTPClient().Do(httpReq)
	if err != nil {
		return fmt.Errorf("codex image request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("codex image error %d: %s", resp.StatusCode, string(body))
	}

	_, err = io.Copy(w, resp.Body)
	return err
}

func (e *CodexExecutor) doStream(ctx context.Context, req *types.ChatCompletionRequest, w io.Writer) error {
	tokenData := e.oauth.GetTokenData(ctx)
	if tokenData == nil {
		return fmt.Errorf("codex not authenticated")
	}
	token := tokenData.AccessToken

	// Auto-refresh stale quota (>12h) in background
	if auth.QuotaCache.IsStale("codex:"+tokenData.ID, 12*time.Hour) {
		go e.oauth.FetchQuotaForAccountByID(context.Background(), tokenData.ID)
	}

	cr := e.toCodexRequest(req)
	body, _ := json.Marshal(cr)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, codexBaseURL+"/codex/responses", bytes.NewReader(body))
	if err != nil {
		return err
	}

	installationID := uuid.New().String()
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("User-Agent", codexUserAgent)
	httpReq.Header.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
	httpReq.Header.Set("x-openai-internal-codex-residency", "us")
	httpReq.Header.Set("x-codex-installation-id", installationID)
	httpReq.Header.Set("x-client-request-id", uuid.New().String())

	resp, err := internaltls.NewAnthropicHTTPClient().Do(httpReq)
	if err != nil {
		return fmt.Errorf("codex request: %w", err)
	}
	defer resp.Body.Close()

	// Extract quota from response headers and store per-account
	if quota := auth.ParseCodexRateLimitHeaders(resp.Header); quota != nil {
		jwtInfo := auth.ParseJWT(tokenData.AccessToken)
		quota.AccountID = tokenData.ID
		quota.Email = tokenData.Email
		if quota.Email == "" {
			quota.Email = jwtInfo.Email
		}
		quota.PlanType = jwtInfo.PlanType
		auth.QuotaCache.Set("codex:"+tokenData.ID, quota)
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("codex error %d: %s", resp.StatusCode, string(respBody))
	}

	_, err = io.Copy(w, resp.Body)
	return err
}

func (e *CodexExecutor) OpenResponsesStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	token, err := e.oauth.GetToken(ctx)
	if err != nil {
		return nil, err
	}

	var reqMap map[string]interface{}
	json.Unmarshal(body, &reqMap)
	reqMap["stream"] = true
	reqMap["store"] = false
	if _, ok := reqMap["instructions"]; !ok {
		reqMap["instructions"] = ""
	}
	patchedBody, _ := json.Marshal(reqMap)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, codexBaseURL+"/codex/responses", bytes.NewReader(patchedBody))
	if err != nil {
		return nil, err
	}

	installationID := uuid.New().String()
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("User-Agent", codexUserAgent)
	httpReq.Header.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
	httpReq.Header.Set("x-openai-internal-codex-residency", "us")
	httpReq.Header.Set("x-codex-installation-id", installationID)
	httpReq.Header.Set("x-client-request-id", uuid.New().String())

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("codex request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("codex error %d: %s", resp.StatusCode, string(respBody))
	}

	return resp.Body, nil
}

// sseTranslator translates Codex SSE events to OpenAI chat.completion.chunk format on the fly
type sseTranslator struct {
	w             io.Writer
	model         string
	chunkID       string
	created       int64
	initialized   bool
	hasToolCalls  bool
	toolCallIndex int
}

func (t *sseTranslator) Write(p []byte) (int, error) {
	if !t.initialized {
		t.chunkID = fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:24])
		t.created = time.Now().Unix()
		t.initialized = true

		writeSSEChunk(t.w, types.ChatCompletionChunk{
			ID: t.chunkID, Object: "chat.completion.chunk", Created: t.created, Model: t.model,
			Choices: []types.ChatCompletionChoice{
				{Index: 0, Delta: &types.ChatResult{Role: "assistant"}},
			},
		})
	}

	scanner := bufio.NewScanner(bytes.NewReader(p))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			continue
		}

		var event map[string]interface{}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		eventType, _ := event["type"].(string)
		switch eventType {
		case "response.output_text.delta":
			if delta, ok := event["delta"].(string); ok {
				writeSSEChunk(t.w, types.ChatCompletionChunk{
					ID: t.chunkID, Object: "chat.completion.chunk", Created: t.created, Model: t.model,
					Choices: []types.ChatCompletionChoice{
						{Index: 0, Delta: &types.ChatResult{Content: delta}},
					},
				})
			}

		case "response.function_call_arguments.start":
			t.hasToolCalls = true
			name, _ := event["name"].(string)
			callID, _ := event["call_id"].(string)
			tc := types.ToolCall{
				ID:   callID,
				Type: "function",
				Function: types.ToolCallFunction{Name: name},
			}
			writeSSEChunk(t.w, types.ChatCompletionChunk{
				ID: t.chunkID, Object: "chat.completion.chunk", Created: t.created, Model: t.model,
				Choices: []types.ChatCompletionChoice{
					{Index: 0, Delta: &types.ChatResult{ToolCalls: []types.ToolCall{tc}}},
				},
			})
			t.toolCallIndex++

		case "response.function_call_arguments.delta":
			if delta, ok := event["delta"].(string); ok {
				tc := types.ToolCall{
					Function: types.ToolCallFunction{Arguments: delta},
				}
				writeSSEChunk(t.w, types.ChatCompletionChunk{
					ID: t.chunkID, Object: "chat.completion.chunk", Created: t.created, Model: t.model,
					Choices: []types.ChatCompletionChoice{
						{Index: 0, Delta: &types.ChatResult{ToolCalls: []types.ToolCall{tc}}},
					},
				})
			}

		case "response.completed":
			finishReason := "stop"
			if t.hasToolCalls {
				finishReason = "tool_calls"
			}
			writeSSEChunk(t.w, types.ChatCompletionChunk{
				ID: t.chunkID, Object: "chat.completion.chunk", Created: t.created, Model: t.model,
				Choices: []types.ChatCompletionChoice{
					{Index: 0, Delta: &types.ChatResult{}, FinishReason: &finishReason},
				},
			})
			fmt.Fprint(t.w, "data: [DONE]\n\n")
		}
	}

	return len(p), nil
}
