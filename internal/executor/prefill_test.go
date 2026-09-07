package executor

import (
	"encoding/json"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

// Anthropic rejects a conversation ending in a plain assistant message on the
// reasoning models ("does not support assistant message prefill"). An
// interrupted turn leaves exactly that shape in the client's transcript, so the
// translation has to drop it instead of replaying it upstream.
func TestTrailingAssistantPrefillIsDropped(t *testing.T) {
	req := &types.ChatCompletionRequest{
		Model: "claude-opus-4-6",
		Messages: []types.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"question"`)},
			{Role: "assistant", Content: json.RawMessage(`"partial answer cut off"`)},
		},
	}

	ar := ToAnthropicRequest(req, req.Model)
	if len(ar.Messages) != 1 {
		t.Fatalf("messages = %d, want 1 (the prefill must be dropped)", len(ar.Messages))
	}
	if ar.Messages[0].Role != "user" {
		t.Fatalf("last role = %s, want user", ar.Messages[0].Role)
	}
}

// Several aborted turns in a row collapse the same way.
func TestConsecutiveTrailingAssistantTurnsAreDropped(t *testing.T) {
	req := &types.ChatCompletionRequest{
		Model: "claude-opus-4-6",
		Messages: []types.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"question"`)},
			{Role: "assistant", Content: json.RawMessage(`"first attempt"`)},
			{Role: "assistant", Content: json.RawMessage(`"second attempt"`)},
		},
	}

	ar := ToAnthropicRequest(req, req.Model)
	if len(ar.Messages) != 1 || ar.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v, want just the user turn", ar.Messages)
	}
}

// An assistant turn carrying tool_use is not a prefill: the tool_result that
// answers it follows as a user message, so the exchange must stay intact.
func TestAssistantToolCallIsNotTreatedAsPrefill(t *testing.T) {
	req := &types.ChatCompletionRequest{
		Model: "claude-opus-4-6",
		Messages: []types.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"run it"`)},
			{Role: "assistant", ToolCalls: []types.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: types.ToolCallFunction{Name: "shell", Arguments: `{"cmd":"ls"}`},
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"output"`)},
		},
	}

	ar := ToAnthropicRequest(req, req.Model)
	if len(ar.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (tool exchange preserved)", len(ar.Messages))
	}
	if ar.Messages[len(ar.Messages)-1].Role != "user" {
		t.Fatal("a tool_result is carried as a user message and must end the conversation")
	}
}

// A conversation that is nothing but assistant turns would translate to an empty
// message list, which Anthropic also rejects. Keeping the original at least lets
// the upstream produce a meaningful error.
func TestAllAssistantConversationIsLeftIntact(t *testing.T) {
	req := &types.ChatCompletionRequest{
		Model: "claude-opus-4-6",
		Messages: []types.ChatMessage{
			{Role: "assistant", Content: json.RawMessage(`"only turn"`)},
		},
	}

	ar := ToAnthropicRequest(req, req.Model)
	if len(ar.Messages) != 1 {
		t.Fatalf("messages = %d, want the original kept rather than an empty list", len(ar.Messages))
	}
}

// The cache breakpoint is applied after the prefill is dropped, so it must land
// on the message that actually gets sent.
func TestCacheBreakpointLandsOnRemainingLastMessage(t *testing.T) {
	req := &types.ChatCompletionRequest{
		Model: "claude-opus-4-6",
		Messages: []types.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"question"`)},
			{Role: "assistant", Content: json.RawMessage(`"answer"`)},
			{Role: "user", Content: json.RawMessage(`"follow up"`)},
			{Role: "assistant", Content: json.RawMessage(`"interrupted"`)},
		},
	}

	ar := ToAnthropicRequest(req, req.Model)
	ApplyCacheBreakpoints(ar)

	last := ar.Messages[len(ar.Messages)-1]
	if last.Role != "user" {
		t.Fatalf("last role = %s, want user", last.Role)
	}
	if !hasCacheControl(t, last.Content) {
		t.Fatal("the breakpoint must be on the last message actually sent")
	}
}

func hasCacheControl(t *testing.T, raw json.RawMessage) bool {
	t.Helper()
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return false
	}
	for _, b := range blocks {
		if _, ok := b["cache_control"]; ok {
			return true
		}
	}
	return false
}
