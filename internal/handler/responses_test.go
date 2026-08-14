package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
	"github.com/gin-gonic/gin"
)

func TestResponsesRequestConvertsCodexContentAndToolsToChatCompletions(t *testing.T) {
	input := json.RawMessage(`[
		{"role":"user","content":[{"type":"input_text","text":"inspect this repository"}]},
		{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"pwd\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"/tmp/project"}
	]`)
	tool := json.RawMessage(`{"type":"function","name":"shell","description":"Run a shell command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}`)
	req := &responsesRequest{
		Model: "kimi-k3",
		Input: input,
		Tools: []json.RawMessage{tool},
	}

	chatReq := (&ResponsesHandler{}).toChatCompletionRequest(req)
	if len(chatReq.Messages) != 3 {
		t.Fatalf("messages = %+v", chatReq.Messages)
	}

	var content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(chatReq.Messages[0].Content, &content); err != nil {
		t.Fatalf("decode converted content: %v", err)
	}
	if len(content) != 1 || content[0].Type != "text" || content[0].Text != "inspect this repository" {
		t.Fatalf("converted content = %+v", content)
	}

	if len(chatReq.Tools) != 1 {
		t.Fatalf("tools = %+v", chatReq.Tools)
	}
	if got := chatReq.Tools[0].Function.Name; got != "shell" {
		t.Fatalf("tool name = %q", got)
	}
	if len(chatReq.Tools[0].Function.Parameters) == 0 {
		t.Fatal("tool parameters were dropped")
	}
}

func TestResponsesAdaptsNonStreamingAnyGenTextToSSE(t *testing.T) {
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")
	var upstreamCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		if r.URL.Path != "/api/v1/chat/completions" {
			t.Fatalf("path = %q, want /api/v1/chat/completions", r.URL.Path)
		}
		var req types.ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if req.Stream {
			t.Fatal("AnyGen upstream request must remain non-streaming")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-anygen","object":"chat.completion","created":1,"model":"gpt-5.6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"hello from AnyGen"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":4,"total_tokens":16}}`)
	}))
	defer server.Close()

	anygenExec := executor.NewAnyGenExecutor(config.AnyGenConfig{
		Enabled:   true,
		BaseURL:   server.URL + "/api/v1",
		APIKeyEnv: "TEST_ANYGEN_LLM_KEY",
		Models:    []string{"gpt-5.6-luna"},
	})
	r := router.New()
	r.Register(anygenExec, "anygen")
	h := NewResponsesHandler(r, nil)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"gpt-5.6-luna",
		"stream":true,
		"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]
	}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.HandleResponses(c)

	if upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	for _, want := range []string{
		"event: response.created",
		"event: response.output_item.added",
		`"delta":"hello from AnyGen"`,
		"event: response.output_text.done",
		"event: response.output_item.done",
		"event: response.completed",
		`"input_tokens":12`,
		`"output_tokens":4`,
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("SSE response missing %q:\n%s", want, w.Body.String())
		}
	}
	assertTextInOrder(t, w.Body.String(),
		"event: response.created",
		"event: response.in_progress",
		"event: response.output_item.added",
		"event: response.content_part.added",
		"event: response.output_text.delta",
		"event: response.output_text.done",
		"event: response.content_part.done",
		"event: response.output_item.done",
		"event: response.completed",
	)
	if got := strings.Count(w.Body.String(), "event: response.created"); got != 1 {
		t.Fatalf("response.created count = %d, want 1", got)
	}
	assertConsistentResponseID(t, w.Body.String())
}

func TestResponsesAdaptsNonStreamingAnyGenToolCallToSSE(t *testing.T) {
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-anygen","object":"chat.completion","created":1,"model":"gpt-5.6-luna","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"cmd\":\"pwd\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28}}`)
	}))
	defer server.Close()

	anygenExec := executor.NewAnyGenExecutor(config.AnyGenConfig{
		BaseURL:   server.URL + "/api/v1",
		APIKeyEnv: "TEST_ANYGEN_LLM_KEY",
		Models:    []string{"gpt-5.6-luna"},
	})
	r := router.New()
	r.Register(anygenExec, "anygen")
	h := NewResponsesHandler(r, nil)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"gpt-5.6-luna",
		"stream":true,
		"input":[{"role":"user","content":[{"type":"input_text","text":"where am I?"}]}],
		"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]
	}`))
	h.HandleResponses(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	for _, want := range []string{
		"event: response.output_item.added",
		`"type":"function_call"`,
		`"call_id":"call_1"`,
		`"name":"shell"`,
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		`"arguments":"{\"cmd\":\"pwd\"}"`,
		"event: response.output_item.done",
		"event: response.completed",
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("SSE response missing %q:\n%s", want, w.Body.String())
		}
	}
	assertTextInOrder(t, w.Body.String(),
		"event: response.output_item.added",
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		"event: response.output_item.done",
		"event: response.completed",
	)
	assertConsistentResponseID(t, w.Body.String())
}

func TestResponsesNonStreamingAdapterReturnsUpstreamErrorBeforeSSE(t *testing.T) {
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"quota exhausted"}`)
	}))
	defer server.Close()

	anygenExec := executor.NewAnyGenExecutor(config.AnyGenConfig{
		BaseURL:   server.URL + "/api/v1",
		APIKeyEnv: "TEST_ANYGEN_LLM_KEY",
		Models:    []string{"gpt-5.6-luna"},
	})
	r := router.New()
	r.Register(anygenExec, "anygen")
	h := NewResponsesHandler(r, nil)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"gpt-5.6-luna",
		"stream":true,
		"input":[{"role":"user","content":"hello"}]
	}`))
	h.HandleResponses(c)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, error must be returned before SSE starts", got)
	}
	if !strings.Contains(w.Body.String(), "quota exhausted") {
		t.Fatalf("error body = %s", w.Body.String())
	}
}

func TestResponsesNonStreamingAdapterRejectsEmptyResponseBeforeSSE(t *testing.T) {
	r := router.New()
	r.Register(emptyNonStreamingExecutor{}, "empty")
	h := NewResponsesHandler(r, nil)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"empty-response-model",
		"stream":true,
		"input":[{"role":"user","content":"hello"}]
	}`))
	h.HandleResponses(c)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, empty response must be rejected before SSE starts", got)
	}
	if !strings.Contains(w.Body.String(), "empty response") {
		t.Fatalf("error body = %s", w.Body.String())
	}
}

func TestChatUsageAsResponsesPreservesTokenDetails(t *testing.T) {
	converted := chatUsageAsResponses(&types.Usage{
		PromptTokens:     20,
		CompletionTokens: 7,
		TotalTokens:      27,
		PromptTokensDetails: &types.PromptTokensDetails{
			CachedTokens:     5,
			CacheWriteTokens: 2,
		},
		CompletionTokensDetails: &types.CompletionTokensDetails{ReasoningTokens: 3},
	})
	payload, err := json.Marshal(converted)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"cached_tokens":5`,
		`"cache_write_tokens":2`,
		`"reasoning_tokens":3`,
	} {
		if !strings.Contains(string(payload), want) {
			t.Errorf("converted usage missing %q: %s", want, payload)
		}
	}
}

func assertTextInOrder(t *testing.T, text string, wants ...string) {
	t.Helper()
	remaining := text
	for _, want := range wants {
		idx := strings.Index(remaining, want)
		if idx < 0 {
			t.Fatalf("missing or out-of-order %q:\n%s", want, text)
		}
		remaining = remaining[idx+len(want):]
	}
}

func assertConsistentResponseID(t *testing.T, stream string) {
	t.Helper()
	var responseID string
	for _, block := range strings.Split(stream, "\n\n") {
		var dataLine string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				dataLine = strings.TrimPrefix(line, "data: ")
				break
			}
		}
		if dataLine == "" {
			continue
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(dataLine), &event); err != nil {
			t.Fatalf("decode SSE event: %v\n%s", err, dataLine)
		}
		eventType, _ := event["type"].(string)
		if nested, ok := event["response"].(map[string]interface{}); ok {
			id, _ := nested["id"].(string)
			if responseID == "" {
				responseID = id
			}
			if id != responseID {
				t.Fatalf("%s response id = %q, want %q", eventType, id, responseID)
			}
			continue
		}
		if strings.HasPrefix(eventType, "response.output_") ||
			strings.HasPrefix(eventType, "response.content_") ||
			strings.HasPrefix(eventType, "response.function_call_") {
			if id, _ := event["response_id"].(string); id == "" || id != responseID {
				t.Fatalf("%s response_id = %q, want %q", eventType, id, responseID)
			}
		}
	}
	if responseID == "" {
		t.Fatal("stream did not include a response id")
	}
}

type emptyNonStreamingExecutor struct{}

func (emptyNonStreamingExecutor) Execute(context.Context, *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	return nil, nil
}

func (emptyNonStreamingExecutor) ExecuteStream(context.Context, *types.ChatCompletionRequest, io.Writer) (*types.Usage, error) {
	return nil, nil
}

func (emptyNonStreamingExecutor) Models() []string {
	return []string{"empty-response-model"}
}

func (emptyNonStreamingExecutor) SupportsStreaming() bool {
	return false
}
