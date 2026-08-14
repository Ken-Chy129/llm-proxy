package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

func TestAnyGenExecutorExecuteUsesConfiguredKeyAndNonStreamingEndpoint(t *testing.T) {
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")

	var gotAuthorization string
	var gotRequest types.ChatCompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/chat/completions" {
			t.Fatalf("path = %q, want /api/v1/chat/completions", r.URL.Path)
		}
		gotAuthorization = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotRequest); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-anygen","object":"chat.completion","created":1,"model":"gpt-5.6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	}))
	defer server.Close()

	exec := NewAnyGenExecutor(config.AnyGenConfig{
		Enabled:   true,
		BaseURL:   server.URL + "/api/v1/",
		APIKeyEnv: "TEST_ANYGEN_LLM_KEY",
	})
	content, _ := json.Marshal("hello")
	resp, err := exec.Execute(context.Background(), &types.ChatCompletionRequest{
		Model:    "gpt-5.6-luna",
		Messages: []types.ChatMessage{{Role: "user", Content: content}},
	})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if gotAuthorization != "Bearer sk-ag-test" {
		t.Fatalf("Authorization = %q", gotAuthorization)
	}
	if gotRequest.Model != "gpt-5.6-luna" || gotRequest.Stream {
		t.Fatalf("upstream request = %+v", gotRequest)
	}
	if resp.Model != "gpt-5.6-luna" || resp.Choices[0].Message.Content != "ok" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestAnyGenExecutorFetchModelsUsesOpenAIModelsEndpoint(t *testing.T) {
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/models" {
			t.Fatalf("request = %s %s, want GET /api/v1/models", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-ag-test" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[{"id":"gpt-5.6-luna","object":"model","owned_by":"anygen"},{"id":"claude-sonnet-4-6","object":"model","owned_by":"anygen"}]}`)
	}))
	defer server.Close()

	exec := NewAnyGenExecutor(config.AnyGenConfig{
		BaseURL:   server.URL + "/api/v1",
		APIKeyEnv: "TEST_ANYGEN_LLM_KEY",
	})
	models, err := exec.SyncModels(context.Background())
	if err != nil {
		t.Fatalf("SyncModels() error: %v", err)
	}
	want := []string{"gpt-5.6-luna", "claude-sonnet-4-6"}
	if !slices.Equal(models, want) || !slices.Equal(exec.Models(), want) {
		t.Fatalf("models = %v, executor models = %v, want %v", models, exec.Models(), want)
	}
}

func TestAnyGenExecutorRefreshCreditsUsesPlatformVerifyEndpoint(t *testing.T) {
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/openapi/key/verify" {
			t.Fatalf("request = %s %s, want GET /v1/openapi/key/verify", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-ag-test" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"verified":true,"user_id":"7530827736760778771","credits":"52141"}`)
	}))
	defer server.Close()

	exec := NewAnyGenExecutor(config.AnyGenConfig{APIKeyEnv: "TEST_ANYGEN_LLM_KEY"})
	exec.verifyURL = server.URL + "/v1/openapi/key/verify"
	credits, err := exec.RefreshCredits(context.Background())
	if err != nil {
		t.Fatalf("RefreshCredits() error: %v", err)
	}
	if !credits.Verified || credits.UserID != "7530827736760778771" || credits.Credits != "52141" {
		t.Fatalf("credits = %+v", credits)
	}
	if cached, ok := exec.Credits(); !ok || cached != credits {
		t.Fatalf("cached credits = %+v, ok=%v", cached, ok)
	}
}

func TestAnyGenExecutorRejectsStreamingWithoutCallingUpstream(t *testing.T) {
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")

	exec := NewAnyGenExecutor(config.AnyGenConfig{APIKeyEnv: "TEST_ANYGEN_LLM_KEY"})
	_, err := exec.ExecuteStream(context.Background(), &types.ChatCompletionRequest{
		Model:  "gpt-5.6-luna",
		Stream: true,
	}, io.Discard)
	if err == nil {
		t.Fatal("ExecuteStream() succeeded, want a non-streaming-only error")
	}
	if got := StatusFromError(err); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; err=%v", got, err)
	}
	if !strings.Contains(err.Error(), "non-streaming") {
		t.Fatalf("error = %q, want non-streaming explanation", err)
	}
}
