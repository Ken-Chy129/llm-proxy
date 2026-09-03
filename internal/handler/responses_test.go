package handler

import (
	"context"
	"encoding/json"
	"errors"
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

	chatReq, err := (&ResponsesHandler{}).toChatCompletionRequest(req)
	if err != nil {
		t.Fatal(err)
	}
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

func TestResponsesRequestGroupsParallelFunctionCallsIntoOneAssistantMessage(t *testing.T) {
	input := json.RawMessage(`[
		{"role":"user","content":[{"type":"input_text","text":"inspect both files"}]},
		{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.go\"}"},
		{"type":"function_call","call_id":"call_2","name":"read_file","arguments":"{\"path\":\"b.go\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"file a"},
		{"type":"function_call_output","call_id":"call_2","output":"file b"}
	]`)
	req := &responsesRequest{Model: "gpt-5.6-sol", Input: input}

	chatReq, err := (&ResponsesHandler{}).toChatCompletionRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(chatReq.Messages) != 4 {
		t.Fatalf("messages = %+v, want user + one assistant tool-call message + two tool outputs", chatReq.Messages)
	}
	assistant := chatReq.Messages[1]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 2 {
		t.Fatalf("parallel calls were not grouped: %+v", assistant)
	}
	if assistant.ToolCalls[0].ID != "call_1" || assistant.ToolCalls[1].ID != "call_2" {
		t.Fatalf("tool call order = %+v", assistant.ToolCalls)
	}
	if chatReq.Messages[2].Role != "tool" || chatReq.Messages[2].ToolCallID != "call_1" ||
		chatReq.Messages[3].Role != "tool" || chatReq.Messages[3].ToolCallID != "call_2" {
		t.Fatalf("tool outputs = %+v", chatReq.Messages[2:])
	}
}

func TestResponsesRequestNormalizesObjectFunctionCallArguments(t *testing.T) {
	input := json.RawMessage(`[
		{"type":"function_call","call_id":"call_1","name":"shell_command","arguments":{"command":"ls -la","timeout_ms":1000}},
		{"type":"function_call_output","call_id":"call_1","output":"ok"}
	]`)
	req := &responsesRequest{Model: "gpt-5.6-sol", Input: input}

	chatReq, err := (&ResponsesHandler{}).toChatCompletionRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(chatReq.Messages) != 2 || len(chatReq.Messages[0].ToolCalls) != 1 {
		t.Fatalf("messages = %+v", chatReq.Messages)
	}
	arguments := chatReq.Messages[0].ToolCalls[0].Function.Arguments
	if arguments != `{"command":"ls -la","timeout_ms":1000}` {
		t.Fatalf("arguments = %q", arguments)
	}
}

func TestChatToolCallArgumentsAcceptsStructuredJSONAndRejectsScalars(t *testing.T) {
	tests := []struct {
		name    string
		raw     json.RawMessage
		want    string
		wantErr bool
	}{
		{name: "string", raw: json.RawMessage(`"{\"command\":\"pwd\"}"`), want: `{"command":"pwd"}`},
		{name: "object", raw: json.RawMessage(`{ "command": "pwd" }`), want: `{"command":"pwd"}`},
		{name: "array", raw: json.RawMessage(`[ { "path": "a.go" } ]`), want: `[{"path":"a.go"}]`},
		{name: "null", raw: json.RawMessage(`null`), wantErr: true},
		{name: "number", raw: json.RawMessage(`42`), wantErr: true},
		{name: "boolean", raw: json.RawMessage(`true`), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := chatToolCallArguments(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("chatToolCallArguments(%s) = %q, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("chatToolCallArguments(%s) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestResponsesRequestPreservesStructuredFunctionCallOutputAsText(t *testing.T) {
	input := json.RawMessage(`[
		{"type":"function_call","call_id":"call_1","name":"screenshot","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":[
			{"type":"input_text","text":"screenshot ready"},
			{"type":"input_image","image_url":"data:image/png;base64,AAAA"}
		]}
	]`)
	req := &responsesRequest{Model: "gpt-5.6-sol", Input: input}

	chatReq, err := (&ResponsesHandler{}).toChatCompletionRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(chatReq.Messages) != 2 {
		t.Fatalf("messages = %+v", chatReq.Messages)
	}
	var output string
	if err := json.Unmarshal(chatReq.Messages[1].Content, &output); err != nil {
		t.Fatalf("decode tool output: %v; raw=%s", err, chatReq.Messages[1].Content)
	}
	if !strings.Contains(output, "screenshot ready") || !strings.Contains(output, "image") {
		t.Fatalf("structured output was lost: %q", output)
	}
}

func TestResponsesRejectsFunctionCallWithoutOutputBeforeAnyGen(t *testing.T) {
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")
	upstreamCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-anygen","object":"chat.completion","model":"gpt-5.6-sol","choices":[{"index":0,"message":{"role":"assistant","content":"unexpected"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	anygenExec := executor.NewAnyGenExecutor(config.AnyGenConfig{
		BaseURL:   server.URL + "/api/v1",
		APIKeyEnv: "TEST_ANYGEN_LLM_KEY",
	})
	anygenExec.SetServed([]string{"gpt-5.6-sol"})
	r := router.New()
	r.SetProvider("anygen", anygenExec)
	r.SetRoutes([]router.Route{{Model: "gpt-5.6-sol", Providers: []string{"anygen"}}})
	h := NewResponsesHandler(r, nil)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"gpt-5.6-sol",
		"stream":true,
		"input":[
			{"role":"user","content":[{"type":"input_text","text":"inspect"}]},
			{"type":"function_call","call_id":"call_missing","name":"read_file","arguments":"{}"}
		]
	}`))
	h.HandleResponses(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if upstreamCalls != 0 {
		t.Fatalf("AnyGen was called %d times for invalid tool history", upstreamCalls)
	}
	if !strings.Contains(w.Body.String(), "call_missing") {
		t.Fatalf("error does not identify the unmatched call: %s", w.Body.String())
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
	})
	anygenExec.SetServed([]string{"gpt-5.6-luna"})
	r := router.New()
	r.SetProvider("anygen", anygenExec)
	r.SetRoutes([]router.Route{{Model: "gpt-5.6-luna", Providers: []string{"anygen"}}})
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

func TestResponsesFallsBackFromNativeCodexToAdaptedAnyGenOn429(t *testing.T) {
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")
	anygenCalls := 0
	anygenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anygenCalls++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-anygen","object":"chat.completion","created":1,"model":"gpt-5.6-sol","choices":[{"index":0,"message":{"role":"assistant","content":"fallback from AnyGen"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":3,"total_tokens":11}}`)
	}))
	defer anygenServer.Close()

	codex := &nativeResponsesStub{
		err: &executor.HTTPError{Backend: "codex", Status: http.StatusTooManyRequests, Body: `{"error":"usage_limit_reached"}`},
	}
	anygen := executor.NewAnyGenExecutor(config.AnyGenConfig{
		BaseURL:   anygenServer.URL,
		APIKeyEnv: "TEST_ANYGEN_LLM_KEY",
	})
	anygen.SetServed([]string{"gpt-5.6-sol"})

	r := router.New()
	r.SetProvider("codex", codex)
	r.SetProvider("anygen", anygen)
	r.SetRoutes([]router.Route{{
		Model:     "gpt-5.6-sol",
		Providers: []string{"codex", "anygen"},
	}})
	h := NewResponsesHandler(r, nil)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"gpt-5.6-sol",
		"stream":true,
		"input":[{"role":"user","content":"hello"}]
	}`))
	h.HandleResponses(c)

	if codex.responsesCalls != 1 {
		t.Fatalf("Codex Responses calls = %d, want 1", codex.responsesCalls)
	}
	if anygenCalls != 1 {
		t.Fatalf("AnyGen calls = %d, want 1 after Codex 429; body=%s", anygenCalls, w.Body.String())
	}
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "fallback from AnyGen") {
		t.Fatalf("status=%d body=%s, want adapted AnyGen success", w.Code, w.Body.String())
	}
}

func TestResponsesUsesAdaptedAnyGenBeforeNativeCodexWhenConfiguredFirst(t *testing.T) {
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")
	anygenCalls := 0
	anygenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anygenCalls++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-anygen","object":"chat.completion","created":1,"model":"gpt-5.6-sol","choices":[{"index":0,"message":{"role":"assistant","content":"AnyGen was first"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	}))
	defer anygenServer.Close()

	anygen := executor.NewAnyGenExecutor(config.AnyGenConfig{
		BaseURL:   anygenServer.URL,
		APIKeyEnv: "TEST_ANYGEN_LLM_KEY",
	})
	anygen.SetServed([]string{"gpt-5.6-sol"})
	codex := &nativeResponsesStub{
		body: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-5.6-sol\"}}\n\n",
	}

	r := router.New()
	r.SetProvider("anygen", anygen)
	r.SetProvider("codex", codex)
	r.SetRoutes([]router.Route{{
		Model:     "gpt-5.6-sol",
		Providers: []string{"anygen", "codex"},
	}})
	h := NewResponsesHandler(r, nil)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"gpt-5.6-sol",
		"stream":true,
		"input":[{"role":"user","content":"hello"}]
	}`))
	h.HandleResponses(c)

	if anygenCalls != 1 {
		t.Fatalf("AnyGen calls = %d, want configured first provider to run; body=%s", anygenCalls, w.Body.String())
	}
	if codex.responsesCalls != 0 {
		t.Fatalf("Codex Responses calls = %d, want 0 when AnyGen succeeds first", codex.responsesCalls)
	}
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "AnyGen was first") {
		t.Fatalf("status=%d body=%s, want adapted AnyGen success", w.Code, w.Body.String())
	}
}

func TestResponsesFallsBackFromAdaptedAnyGenToNativeCodexOn429(t *testing.T) {
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")
	anygenCalls := 0
	anygenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anygenCalls++
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"quota exhausted"}`)
	}))
	defer anygenServer.Close()

	anygen := executor.NewAnyGenExecutor(config.AnyGenConfig{
		BaseURL:   anygenServer.URL,
		APIKeyEnv: "TEST_ANYGEN_LLM_KEY",
	})
	anygen.SetServed([]string{"gpt-5.6-sol"})
	codex := &nativeResponsesStub{
		body: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-5.6-sol\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"fallback from Codex\"}]}]}}\n\n",
	}

	r := router.New()
	r.SetProvider("anygen", anygen)
	r.SetProvider("codex", codex)
	r.SetRoutes([]router.Route{{
		Model:     "gpt-5.6-sol",
		Providers: []string{"anygen", "codex"},
	}})
	h := NewResponsesHandler(r, nil)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"gpt-5.6-sol",
		"stream":true,
		"input":[{"role":"user","content":"hello"}]
	}`))
	h.HandleResponses(c)

	if anygenCalls != 1 {
		t.Fatalf("AnyGen calls = %d, want 1", anygenCalls)
	}
	if codex.responsesCalls != 1 {
		t.Fatalf("Codex Responses calls = %d, want 1 after AnyGen 429; body=%s", codex.responsesCalls, w.Body.String())
	}
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "fallback from Codex") {
		t.Fatalf("status=%d body=%s, want native Codex success", w.Code, w.Body.String())
	}
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
	})
	anygenExec.SetServed([]string{"gpt-5.6-luna"})
	r := router.New()
	r.SetProvider("anygen", anygenExec)
	r.SetRoutes([]router.Route{{Model: "gpt-5.6-luna", Providers: []string{"anygen"}}})
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

func TestResponsesStreamingAdapterCompletesToolCallSSE(t *testing.T) {
	var output strings.Builder
	_, err := (&ResponsesHandler{}).streamWithTranslation(
		context.Background(),
		streamingToolCallExecutor{},
		&types.ChatCompletionRequest{Model: "claude-fable-5-1"},
		&output,
	)
	if err != nil {
		t.Fatal(err)
	}

	stream := output.String()
	for _, want := range []string{
		`"output_index":0`,
		`"call_id":"toolu_1"`,
		`"name":"exec_command"`,
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		`"arguments":"{\"cmd\":\"pwd\"}"`,
		`"status":"completed"`,
		"event: response.output_item.done",
		"event: response.completed",
	} {
		if !strings.Contains(stream, want) {
			t.Errorf("SSE response missing %q:\n%s", want, stream)
		}
	}
	assertTextInOrder(t, stream,
		"event: response.output_item.added",
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		"event: response.output_item.done",
		"event: response.completed",
	)
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
	})
	anygenExec.SetServed([]string{"gpt-5.6-luna"})
	r := router.New()
	r.SetProvider("anygen", anygenExec)
	r.SetRoutes([]router.Route{{Model: "gpt-5.6-luna", Providers: []string{"anygen"}}})
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
	r.SetProvider("empty", emptyNonStreamingExecutor{})
	r.SetRoutes([]router.Route{{Model: "empty-response-model", Providers: []string{"empty"}}})
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

type streamingToolCallExecutor struct{}

func (streamingToolCallExecutor) Execute(context.Context, *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	return nil, errors.New("non-streaming execution is not expected")
}

func (streamingToolCallExecutor) ExecuteStream(_ context.Context, _ *types.ChatCompletionRequest, w io.Writer) (*types.Usage, error) {
	io.WriteString(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"claude-fable-5-1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
	io.WriteString(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"claude-fable-5-1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"toolu_1\",\"type\":\"function\",\"function\":{\"name\":\"exec_command\",\"arguments\":\"\"}}]}}]}\n\n")
	io.WriteString(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"claude-fable-5-1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"{\\\"cmd\\\":\\\"pw\"}}]}}]}\n\n")
	io.WriteString(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"claude-fable-5-1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"d\\\"}\"}}]}}]}\n\n")
	io.WriteString(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"claude-fable-5-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
	io.WriteString(w, "data: [DONE]\n\n")
	return nil, nil
}

func (streamingToolCallExecutor) Models() []string {
	return []string{"claude-fable-5-1"}
}

type nativeResponsesStub struct {
	responsesCalls int
	body           string
	err            error
}

func (s *nativeResponsesStub) Models() []string { return []string{"gpt-5.6-sol"} }

func (s *nativeResponsesStub) Execute(context.Context, *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	return nil, errors.New("chat adapter should not be called for native Responses")
}

func (s *nativeResponsesStub) ExecuteStream(context.Context, *types.ChatCompletionRequest, io.Writer) (*types.Usage, error) {
	return nil, errors.New("chat streaming adapter should not be called for native Responses")
}

func (s *nativeResponsesStub) OpenResponsesStream(context.Context, []byte) (io.ReadCloser, error) {
	s.responsesCalls++
	if s.err != nil {
		return nil, s.err
	}
	return io.NopCloser(strings.NewReader(s.body)), nil
}
