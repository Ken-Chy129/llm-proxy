package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/executor"
)

// End-to-end guard for the 400 that surfaced as
// "messages.N.content.0.tool_use.input: Field required": a Codex client calls a
// parameterless tool, so `arguments` arrives empty, and the body handed to
// Anthropic must still carry an `input` on that tool_use block.
func TestResponsesParameterlessToolCallKeepsAnthropicInput(t *testing.T) {
	for _, tc := range []struct {
		name      string
		arguments string
	}{
		{"omitted", ""},
		{"empty string", `,"arguments":""`},
		{"empty object", `,"arguments":"{}"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := json.RawMessage(`[
				{"role":"user","content":[{"type":"input_text","text":"what time is it"}]},
				{"type":"function_call","call_id":"call_1","name":"get_time"` + tc.arguments + `},
				{"type":"function_call_output","call_id":"call_1","output":"12:00"}
			]`)
			req := &responsesRequest{Model: "claude-sonnet-4", Input: input}

			chatReq, err := (&ResponsesHandler{}).toChatCompletionRequest(req)
			if err != nil {
				t.Fatalf("convert responses request: %v", err)
			}

			ar := executor.ToAnthropicRequest(chatReq, "claude-sonnet-4")
			executor.ApplyCacheBreakpoints(ar)

			body, err := json.Marshal(ar)
			if err != nil {
				t.Fatalf("marshal anthropic body: %v", err)
			}

			var decoded struct {
				Messages []struct {
					Role    string `json:"role"`
					Content []struct {
						Type  string          `json:"type"`
						Name  string          `json:"name"`
						Input json.RawMessage `json:"input"`
					} `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("decode anthropic body %s: %v", body, err)
			}

			var seen int
			for _, msg := range decoded.Messages {
				for _, block := range msg.Content {
					if block.Type != "tool_use" {
						continue
					}
					seen++
					if len(block.Input) == 0 {
						t.Fatalf("tool_use %q has no input field: %s", block.Name, body)
					}
					if string(block.Input) == "null" {
						t.Fatalf("tool_use %q has a null input: %s", block.Name, body)
					}
				}
			}
			if seen != 1 {
				t.Fatalf("expected exactly 1 tool_use block, saw %d: %s", seen, body)
			}
			if strings.Contains(string(body), `"content":null`) {
				t.Fatalf("a message serialised to null content: %s", body)
			}
		})
	}
}
