package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
)

func TestRelayExecutorDefaultsToVerifiedClaudeModels(t *testing.T) {
	exec := NewRelayExecutor(config.RelayConfig{})

	want := []string{
		"claude-opus-5",
		"claude-fable-5",
		"claude-sonnet-4-5-20250929",
		"claude-opus-4-5-20251101",
		"claude-haiku-4-5-20251001",
	}
	if got := exec.Models(); !slices.Equal(got, want) {
		t.Fatalf("Models() = %v, want %v", got, want)
	}
}

func TestRelayExecutorPassesClaudeCodeRequestToAnthropicUpstream(t *testing.T) {
	t.Setenv("TEST_RELAY_AUTH_TOKEN", "relay-secret")

	var gotPath string
	var gotAPIKey string
	var gotVersion string
	var gotBody map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_test","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-4-5-20250929","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}`)
	}))
	defer server.Close()

	exec := NewRelayExecutor(config.RelayConfig{
		Enabled:      true,
		BaseURL:      server.URL,
		AuthTokenEnv: "TEST_RELAY_AUTH_TOKEN",
		Models: []config.ModelConfig{{
			Name:  "relay-sonnet",
			Model: "claude-sonnet-4-5-20250929",
		}},
	})
	body := []byte(`{"model":"relay-sonnet","max_tokens":32,"context_management":{"edits":[]},"messages":[{"role":"user","content":"hello"}]}`)
	responseBody, status, err := exec.ExecuteAnthropicRaw(context.Background(), body, http.Header{
		"anthropic-version": []string{"2023-06-01"},
	})
	if err != nil {
		t.Fatalf("ExecuteAnthropicRaw() error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, responseBody)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("upstream path = %q, want /v1/messages", gotPath)
	}
	if gotAPIKey != "relay-secret" {
		t.Fatalf("x-api-key = %q, want configured auth token", gotAPIKey)
	}
	if gotVersion != "2023-06-01" {
		t.Fatalf("anthropic-version = %q", gotVersion)
	}
	var model string
	json.Unmarshal(gotBody["model"], &model)
	if model != "claude-sonnet-4-5-20250929" {
		t.Fatalf("upstream model = %q", model)
	}
	if _, ok := gotBody["context_management"]; !ok {
		t.Fatal("Claude Code extension field was dropped")
	}
}
