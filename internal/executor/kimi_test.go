package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

func TestKimiExecutorExecuteUsesConfiguredKeyAndModelMapping(t *testing.T) {
	t.Setenv("TEST_MOONSHOT_API_KEY", "secret-for-test")

	var gotAuthorization string
	var gotModel string
	var gotReasoningEffort string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		var body struct {
			Model           string `json:"model"`
			ReasoningEffort string `json:"reasoning_effort"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		gotModel = body.Model
		gotReasoningEffort = body.ReasoningEffort
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"kimi-k3","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	}))
	defer server.Close()

	exec := NewKimiExecutor(config.KimiConfig{
		Enabled:   true,
		BaseURL:   server.URL + "/v1",
		APIKeyEnv: "TEST_MOONSHOT_API_KEY",
		Models: []config.ModelConfig{
			{Name: "kimi-code", Model: "kimi-k3"},
		},
	})

	content, _ := json.Marshal("hello")
	resp, err := exec.Execute(context.Background(), &types.ChatCompletionRequest{
		Model:           "kimi-code",
		Messages:        []types.ChatMessage{{Role: "user", Content: content}},
		ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if gotAuthorization != "Bearer secret-for-test" {
		t.Fatalf("Authorization = %q", gotAuthorization)
	}
	if gotModel != "kimi-k3" {
		t.Fatalf("upstream model = %q, want kimi-k3", gotModel)
	}
	if gotReasoningEffort != "max" {
		t.Fatalf("upstream reasoning_effort = %q, want max for kimi-k3", gotReasoningEffort)
	}
	if resp.Model != "kimi-code" {
		t.Fatalf("response model = %q, want client alias", resp.Model)
	}
	if got := resp.Choices[0].Message.Content; got != "ok" {
		t.Fatalf("response content = %q", got)
	}
}

func TestKimiExecutorTranslatesAnthropicMessagesToChatCompletions(t *testing.T) {
	t.Setenv("TEST_MOONSHOT_API_KEY", "secret-for-test")

	var upstream types.ChatCompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&upstream); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"kimi-k3","choices":[{"index":0,"message":{"role":"assistant","content":"hello from kimi","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"README.md\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}`)
	}))
	defer server.Close()

	exec := NewKimiExecutor(config.KimiConfig{
		Enabled:   true,
		BaseURL:   server.URL + "/v1",
		APIKeyEnv: "TEST_MOONSHOT_API_KEY",
		Models:    []config.ModelConfig{{Name: "kimi-k3", Model: "kimi-k3"}},
	})

	body := []byte(`{
		"model":"kimi-k3",
		"max_tokens":1024,
		"system":[{"type":"text","text":"You are a coding agent."}],
		"messages":[{"role":"user","content":[{"type":"text","text":"Inspect the README"}]}],
		"tools":[{"name":"read_file","description":"Read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]
	}`)
	responseBody, status, err := exec.ExecuteAnthropicRaw(context.Background(), body, nil)
	if err != nil {
		t.Fatalf("ExecuteAnthropicRaw() error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}

	if upstream.Model != "kimi-k3" || len(upstream.Messages) != 2 {
		t.Fatalf("unexpected upstream request: %+v", upstream)
	}
	if len(upstream.Tools) != 1 || upstream.Tools[0].Function.Name != "read_file" {
		t.Fatalf("upstream tools = %+v", upstream.Tools)
	}

	var response types.AnthropicResponse
	if err := json.Unmarshal(responseBody, &response); err != nil {
		t.Fatalf("decode anthropic response: %v", err)
	}
	if response.Type != "message" || response.Role != "assistant" {
		t.Fatalf("unexpected response envelope: %+v", response)
	}
	if response.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q", response.StopReason)
	}
	if len(response.Content) != 2 || response.Content[1].Type != "tool_use" {
		t.Fatalf("content blocks = %+v", response.Content)
	}
	if response.Usage.InputTokens != 12 || response.Usage.OutputTokens != 7 {
		t.Fatalf("usage = %+v", response.Usage)
	}
}

func TestKimiExecutorTranslatesChatStreamToAnthropicSSE(t *testing.T) {
	t.Setenv("TEST_MOONSHOT_API_KEY", "secret-for-test")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"kimi-k3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"kimi-k3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"kimi-k3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	exec := NewKimiExecutor(config.KimiConfig{
		Enabled:   true,
		BaseURL:   server.URL + "/v1",
		APIKeyEnv: "TEST_MOONSHOT_API_KEY",
		Models:    []config.ModelConfig{{Name: "kimi-k3", Model: "kimi-k3"}},
	})

	stream, status, err := exec.OpenAnthropicStream(context.Background(), []byte(`{"model":"kimi-k3","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`), nil)
	if err != nil {
		t.Fatalf("OpenAnthropicStream() error: %v", err)
	}
	defer stream.Close()
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read translated stream: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		`event: message_start`,
		`"type":"content_block_delta"`,
		`"text":"hello"`,
		`"stop_reason":"end_turn"`,
		`event: message_stop`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("translated stream missing %q:\n%s", want, got)
		}
	}
}

func TestKimiExecutorTranslatesToolCallStreamToAnthropicSSE(t *testing.T) {
	t.Setenv("TEST_MOONSHOT_API_KEY", "secret-for-test")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"kimi-k3\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"\"}}]}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"kimi-k3\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"path\\\":\\\"README.md\\\"}\"}}]}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"kimi-k3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	exec := NewKimiExecutor(config.KimiConfig{
		Enabled:   true,
		BaseURL:   server.URL + "/v1",
		APIKeyEnv: "TEST_MOONSHOT_API_KEY",
		Models:    []config.ModelConfig{{Name: "kimi-k3", Model: "kimi-k3"}},
	})

	stream, status, err := exec.OpenAnthropicStream(context.Background(), []byte(`{"model":"kimi-k3","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"read the README"}]}`), nil)
	if err != nil {
		t.Fatalf("OpenAnthropicStream() error: %v", err)
	}
	defer stream.Close()
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read translated stream: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		`"type":"tool_use"`,
		`"name":"read_file"`,
		`"type":"input_json_delta"`,
		`{\"path\":\"README.md\"}`,
		`"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("translated tool stream missing %q:\n%s", want, got)
		}
	}
}
