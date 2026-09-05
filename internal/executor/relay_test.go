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
	"github.com/tidwall/gjson"
)

func TestRelayServesNothingUntilRoutingAssignsModels(t *testing.T) {
	exec := NewRelayExecutor(config.RelayConfig{})
	if got := exec.Models(); len(got) != 0 {
		t.Fatalf("Models() = %v, want empty before routing is applied", got)
	}

	exec.SetModels([]config.ModelConfig{{Name: "claude-opus-5"}})
	if got := exec.Models(); !slices.Equal(got, []string{"claude-opus-5"}) {
		t.Fatalf("Models() = %v, want [claude-opus-5]", got)
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
	})
	exec.SetModels([]config.ModelConfig{{
		Name:  "relay-sonnet",
		Model: "claude-sonnet-4-5-20250929",
	}})
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
	forwarded, _ := json.Marshal(gotBody)
	if strings.Contains(string(forwarded), `"cache_control"`) {
		t.Fatal("native Messages passthrough must not invent cache breakpoints")
	}
}

func TestRelayTranslatedRequestAddsCacheBreakpoints(t *testing.T) {
	exec := NewRelayExecutor(config.RelayConfig{})
	exec.SetModels([]config.ModelConfig{{Name: "relay-sonnet", Model: "claude-sonnet-4-5-20250929"}})

	system, _ := json.Marshal("You are a coding agent.")
	user, _ := json.Marshal("inspect the repository")
	assistant, _ := json.Marshal("I will inspect it.")
	followup, _ := json.Marshal("continue")
	req := &types.ChatCompletionRequest{
		Model: "relay-sonnet",
		Messages: []types.ChatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
			{Role: "assistant", Content: assistant},
			{Role: "user", Content: followup},
		},
		Tools: []types.Tool{{
			Type: "function",
			Function: types.ToolFunction{
				Name:       "shell",
				Parameters: json.RawMessage(`{"type":"object"}`),
			},
		}},
	}

	translated := exec.translatedAnthropicRequest(req, true)
	if translated.Model != "claude-sonnet-4-5-20250929" {
		t.Fatalf("model = %q", translated.Model)
	}
	if !translated.Stream {
		t.Fatal("stream flag was not preserved")
	}
	if translated.Tools[0].CacheControl == nil {
		t.Fatal("relay tool prefix has no cache breakpoint")
	}
	if translated.System[0].CacheControl == nil {
		t.Fatal("relay system prefix has no cache breakpoint")
	}
	last := translated.Messages[len(translated.Messages)-1].Content
	if got := gjson.GetBytes(last, "0.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("relay conversation prefix cache breakpoint = %q; content=%s", got, last)
	}
}

func TestKimiTranslatedRequestDoesNotAddAnthropicCacheBreakpoints(t *testing.T) {
	exec := NewKimiExecutor(config.KimiConfig{APIFormat: "anthropic"})
	system, _ := json.Marshal("You are a coding agent.")
	user, _ := json.Marshal("hello")
	assistant, _ := json.Marshal("hi")
	followup, _ := json.Marshal("continue")
	req := &types.ChatCompletionRequest{
		Model: "kimi-k3",
		Messages: []types.ChatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
			{Role: "assistant", Content: assistant},
			{Role: "user", Content: followup},
		},
		Tools: []types.Tool{{Type: "function", Function: types.ToolFunction{Name: "shell", Parameters: json.RawMessage(`{}`)}}},
	}

	body, err := json.Marshal(exec.translatedAnthropicRequest(req, true))
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(body, "tools.0.cache_control").Exists() ||
		gjson.GetBytes(body, "system.0.cache_control").Exists() ||
		gjson.GetBytes(body, "messages.3.content.0.cache_control").Exists() {
		t.Fatalf("Kimi request unexpectedly received Anthropic cache breakpoints: %s", body)
	}
}
