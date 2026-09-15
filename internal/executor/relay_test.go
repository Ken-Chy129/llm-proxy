package executor

import (
	"context"
	"encoding/json"
	"fmt"
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

func countTopLevelCacheBreakpoints(body []byte) int {
	var value interface{}
	if json.Unmarshal(body, &value) != nil {
		return 0
	}
	var count func(interface{}) int
	count = func(value interface{}) int {
		switch value := value.(type) {
		case map[string]interface{}:
			n := 0
			if value["cache_control"] != nil {
				n++
			}
			for key, child := range value {
				if key != "cache_control" {
					n += count(child)
				}
			}
			return n
		case []interface{}:
			n := 0
			for _, child := range value {
				n += count(child)
			}
			return n
		default:
			return 0
		}
	}
	return count(value)
}

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

func TestRelayPassthroughBridgesLongPromptCacheLookback(t *testing.T) {
	assistantBlocks := make([]map[string]interface{}, 0, 22)
	for i := 0; i < 22; i++ {
		assistantBlocks = append(assistantBlocks, map[string]interface{}{
			"type":      "text",
			"text":      fmt.Sprintf("block-%d", i),
			"extension": fmt.Sprintf("keep-%d", i),
		})
	}
	body, err := json.Marshal(map[string]interface{}{
		"model":      "claude-fable-5-1",
		"max_tokens": 32,
		"context_management": map[string]interface{}{
			"edits": []interface{}{},
		},
		"tools": []map[string]interface{}{{
			"name": "shell", "input_schema": map[string]string{"type": "object"},
			"cache_control": map[string]string{"type": "ephemeral"},
		}},
		"system": []map[string]interface{}{{
			"type": "text", "text": "system",
			"cache_control": map[string]string{"type": "ephemeral"},
		}},
		"messages": []map[string]interface{}{
			{"role": "user", "content": []map[string]interface{}{{"type": "text", "text": "previous request tail"}}},
			{"role": "assistant", "content": assistantBlocks},
			{"role": "user", "content": []map[string]interface{}{{
				"type": "tool_result", "tool_use_id": "toolu_21", "content": "done",
				"cache_control": map[string]string{"type": "ephemeral"},
			}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, diagnostic := addRelayCacheLookbackBridgeWithDiagnostic(body)
	if diagnostic.Action != "bridged" || diagnostic.Reason != "lookback_gap" || diagnostic.Bridge != "message:0/block:0" {
		t.Fatalf("unexpected bridge diagnostic: %s", diagnostic.String())
	}
	if count := countTopLevelCacheBreakpoints(got); count != 4 {
		t.Fatalf("cache breakpoints = %d, want 4: %s", count, got)
	}
	if bridge := gjson.GetBytes(got, "messages.0.content.0.cache_control.type").String(); bridge != "ephemeral" {
		t.Fatalf("bridge cache breakpoint = %q, want ephemeral: %s", bridge, got)
	}
	if tail := gjson.GetBytes(got, "messages.2.content.0.cache_control.type").String(); tail != "ephemeral" {
		t.Fatalf("client tail cache breakpoint was lost: %s", got)
	}
	if extension := gjson.GetBytes(got, "messages.1.content.1.extension").String(); extension != "keep-1" {
		t.Fatalf("unmodelled content field was lost: %s", got)
	}
	if !gjson.GetBytes(got, "context_management.edits").IsArray() {
		t.Fatalf("top-level Claude Code extension was lost: %s", got)
	}

	exec := NewRelayExecutor(config.RelayConfig{})
	exec.SetModels([]config.ModelConfig{{Name: "claude-fable-5-1", Model: "upstream-fable-5-1"}})
	forwarded, err := exec.rewriteAnthropicModel(body)
	if err != nil {
		t.Fatal(err)
	}
	if model := gjson.GetBytes(forwarded, "model").String(); model != "upstream-fable-5-1" {
		t.Fatalf("upstream model = %q", model)
	}
	if bridge := gjson.GetBytes(forwarded, "messages.0.content.0.cache_control.type").String(); bridge != "ephemeral" {
		t.Fatalf("relay production path omitted bridge breakpoint: %s", forwarded)
	}
}

func TestRelayPassthroughExtendsStaleTailBreakpoint(t *testing.T) {
	body := []byte(`{"model":"claude-fable-5-1","tools":[{"name":"shell","cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"system","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"cached","cache_control":{"type":"ephemeral"}}]},{"role":"assistant","content":[{"type":"text","text":"answer"}]},{"role":"user","content":[{"type":"text","text":"next","extension":"keep"}]}]}`)

	got, diagnostic := addRelayCacheLookbackBridgeWithDiagnostic(body)
	if diagnostic.Action != "bridged" || diagnostic.Reason != "stale_tail_breakpoint" || diagnostic.Bridge != "message:2/block:0" {
		t.Fatalf("unexpected tail diagnostic: %s", diagnostic.String())
	}
	if count := countTopLevelCacheBreakpoints(got); count != 4 {
		t.Fatalf("cache breakpoints = %d, want 4: %s", count, got)
	}
	if tail := gjson.GetBytes(got, "messages.2.content.0.cache_control.type").String(); tail != "ephemeral" {
		t.Fatalf("new tail cache breakpoint = %q: %s", tail, got)
	}
	if extension := gjson.GetBytes(got, "messages.2.content.0.extension").String(); extension != "keep" {
		t.Fatalf("tail extension was lost: %s", got)
	}
}

func TestRelayPassthroughLeavesNearbyAndFullCacheBreakpointsAlone(t *testing.T) {
	nearby := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b"},{"type":"text","text":"c","cache_control":{"type":"ephemeral"}}]}]}`)
	if got := addRelayCacheLookbackBridge(nearby); string(got) != string(nearby) {
		t.Fatalf("nearby breakpoint request changed:\n%s", got)
	}

	full := []byte(`{"model":"m","metadata":{"trace":"keep-me"},"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b","cache_control":{"type":"ephemeral"}},{"type":"text","text":"c","cache_control":{"type":"ephemeral"}},{"type":"text","text":"d","cache_control":{"type":"ephemeral"}}]}]}`)
	got, diagnostic := addRelayCacheLookbackBridgeWithDiagnostic(full)
	if string(got) != string(full) {
		t.Fatalf("four-breakpoint request changed:\n%s", got)
	}
	if diagnostic.Action != "skip" || diagnostic.Reason != "breakpoint_limit" {
		t.Fatalf("unexpected full-breakpoint diagnostic: %s", diagnostic.String())
	}
}

func TestRelayCacheDiagnosticReportsShapeWithoutPromptContent(t *testing.T) {
	body := []byte(`{"model":"claude-fable-5-1","tools":[{"name":"secret-tool","cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"secret-system","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"secret-user"}]},{"role":"assistant","content":[{"type":"text","text":"secret-assistant"}]},{"role":"user","content":[{"type":"text","text":"secret-tail","cache_control":{"type":"ephemeral"}}]}]}`)

	report := inspectRelayCacheRequest(body)
	got := report.String()
	for _, want := range []string{
		"model=claude-fable-5-1", "messages=3", "blocks=3",
		"shape=user:1,assistant:1,user:1", "breakpoints=tool:0,system:0,message:2/block:0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostic %q missing %q", got, want)
		}
	}
	for _, secret := range []string{"secret-tool", "secret-system", "secret-user", "secret-assistant", "secret-tail"} {
		if strings.Contains(got, secret) {
			t.Errorf("diagnostic leaked prompt content %q: %s", secret, got)
		}
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
