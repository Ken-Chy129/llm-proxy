package executor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

// toolCallRequest builds a conversation whose assistant turn issues one tool
// call with the given raw arguments.
func toolCallRequest(arguments string) *types.ChatCompletionRequest {
	return &types.ChatCompletionRequest{
		Model: "claude-sonnet-4",
		Messages: []types.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"what time is it"`)},
			{Role: "assistant", ToolCalls: []types.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: types.ToolCallFunction{Name: "get_time", Arguments: arguments},
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"12:00"`)},
		},
	}
}

// Anthropic requires `input` on every tool_use block. A parameterless tool call
// arrives from OpenAI clients with empty arguments, and `omitempty` used to drop
// the field entirely, which upstream rejected with
// "messages.N.content.0.tool_use.input: Field required".
func TestToolUseInputIsAlwaysPresent(t *testing.T) {
	for _, tc := range []struct {
		name      string
		arguments string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"null", "null"},
		{"truncated", `{"tz":`},
		{"not json", "oops"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ar := ToAnthropicRequest(toolCallRequest(tc.arguments), "claude-sonnet-4")

			raw, err := json.Marshal(ar.Messages[1])
			if err != nil {
				t.Fatalf("marshal assistant message: %v", err)
			}
			if !strings.Contains(string(raw), `"input"`) {
				t.Fatalf("tool_use block has no input field: %s", raw)
			}

			blocks := decodeBlocks(t, ar.Messages[1].Content)
			if len(blocks) != 1 || blocks[0].Type != "tool_use" {
				t.Fatalf("blocks = %s, want a single tool_use block", ar.Messages[1].Content)
			}
			if got := string(blocks[0].Input); got != "{}" {
				t.Errorf("input = %q, want %q", got, "{}")
			}
		})
	}
}

// Real arguments must survive untouched.
func TestToolUseInputPreservesRealArguments(t *testing.T) {
	ar := ToAnthropicRequest(toolCallRequest(`{"tz":"Asia/Shanghai"}`), "claude-sonnet-4")

	blocks := decodeBlocks(t, ar.Messages[1].Content)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d: %s", len(blocks), ar.Messages[1].Content)
	}
	var got struct {
		TZ string `json:"tz"`
	}
	if err := json.Unmarshal(blocks[0].Input, &got); err != nil {
		t.Fatalf("unmarshal input %s: %v", blocks[0].Input, err)
	}
	if got.TZ != "Asia/Shanghai" {
		t.Errorf("input.tz = %q, want %q", got.TZ, "Asia/Shanghai")
	}
}

// The whole request must serialise cleanly; invalid arguments used to make
// json.Marshal fail for the entire body, and that error was being discarded.
func TestToAnthropicRequestMarshalsWithBadToolArguments(t *testing.T) {
	ar := ToAnthropicRequest(toolCallRequest(`{"tz":`), "claude-sonnet-4")
	ApplyCacheBreakpoints(ar)

	body, err := json.Marshal(ar)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if strings.Contains(string(body), `"input":null`) {
		t.Errorf("body carries a null input: %s", body)
	}
}
