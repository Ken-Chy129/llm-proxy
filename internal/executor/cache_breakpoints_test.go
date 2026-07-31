package executor

import (
	"encoding/json"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/types"
	"github.com/tidwall/gjson"
)

func chatReq(system string, tools int, msgs ...types.ChatMessage) *types.ChatCompletionRequest {
	req := &types.ChatCompletionRequest{Model: "claude-opus-4-6"}
	if system != "" {
		raw, _ := json.Marshal(system)
		req.Messages = append(req.Messages, types.ChatMessage{Role: "system", Content: raw})
	}
	req.Messages = append(req.Messages, msgs...)
	for i := 0; i < tools; i++ {
		req.Tools = append(req.Tools, types.Tool{
			Type: "function",
			Function: types.ToolFunction{
				Name:       string(rune('a' + i)),
				Parameters: json.RawMessage(`{}`),
			},
		})
	}
	return req
}

func userMsg(text string) types.ChatMessage {
	raw, _ := json.Marshal(text)
	return types.ChatMessage{Role: "user", Content: raw}
}

func assistantMsg(text string) types.ChatMessage {
	raw, _ := json.Marshal(text)
	return types.ChatMessage{Role: "assistant", Content: raw}
}

// The whole point: a translated request must carry breakpoints, or the upstream
// bills the identical prefix at full price on every single call.
func TestApplyCacheBreakpointsMarksStablePrefixes(t *testing.T) {
	ar := ToAnthropicRequest(chatReq("you are a bot", 3, userMsg("hi"), assistantMsg("hello"), userMsg("again")), "claude-opus-4-6")
	ApplyCacheBreakpoints(ar)

	if n := len(ar.Tools); ar.Tools[n-1].CacheControl == nil {
		t.Error("last tool has no breakpoint — the toolset prefix is reusable across conversations, it is the cheapest one to cache")
	}
	for _, tool := range ar.Tools[:len(ar.Tools)-1] {
		if tool.CacheControl != nil {
			t.Error("only the last tool should be marked; earlier ones waste breakpoints (max 4) on prefixes the last one already covers")
		}
	}
	if n := len(ar.System); ar.System[n-1].CacheControl == nil {
		t.Error("last system block has no breakpoint")
	}
	last, _ := json.Marshal(ar.Messages[len(ar.Messages)-1].Content)
	if !gjson.GetBytes(ar.Messages[len(ar.Messages)-1].Content, "0.cache_control.type").Exists() {
		t.Errorf("continuation's last message has no breakpoint: %s", last)
	}
}

// A cache write costs 1.25x, so a one-shot request must not be marked: nothing
// will ever read that prefix back.
func TestApplyCacheBreakpointsSkipsMessagesOnOneShot(t *testing.T) {
	ar := ToAnthropicRequest(chatReq("sys", 1, userMsg("just this once")), "claude-opus-4-6")
	ApplyCacheBreakpoints(ar)

	if gjson.GetBytes(ar.Messages[0].Content, "0.cache_control").Exists() {
		t.Error("one-shot request got a message breakpoint — that is a 25% surcharge for a prefix nobody reads back")
	}
	// The stable prefixes are still worth caching even for a one-shot.
	if ar.System[0].CacheControl == nil || ar.Tools[0].CacheControl == nil {
		t.Error("system/tools breakpoints should apply regardless of conversation shape")
	}
}

// Content is raw JSON because the struct cannot model every accepted shape.
// Marking must not silently drop the parts it does not understand.
func TestMarkLastContentBlockPreservesUnmodelledFields(t *testing.T) {
	raw := json.RawMessage(`[{"type":"tool_result","tool_use_id":"x","content":[{"type":"text","text":"deep"}],"is_error":false}]`)
	out := markLastContentBlock(raw)

	if got := gjson.GetBytes(out, "0.content.0.text").String(); got != "deep" {
		t.Errorf("array-shaped tool_result content was mangled: %s", out)
	}
	if !gjson.GetBytes(out, "0.is_error").Exists() {
		t.Errorf("field absent from AnthropicContentBlock was dropped: %s", out)
	}
	if got := gjson.GetBytes(out, "0.cache_control.type").String(); got != "ephemeral" {
		t.Errorf("breakpoint not set: %s", out)
	}
}

func TestMarkLastContentBlockPromotesStringContent(t *testing.T) {
	out := markLastContentBlock(json.RawMessage(`"plain text"`))
	if got := gjson.GetBytes(out, "0.text").String(); got != "plain text" {
		t.Errorf("text lost: %s", out)
	}
	if got := gjson.GetBytes(out, "0.cache_control.type").String(); got != "ephemeral" {
		t.Errorf("breakpoint not set on promoted block: %s", out)
	}
}

// Regression: the Claude Code identity injection used to stringify the caller's
// whole system array into one text block, destroying its cache_control (and
// paying extra tokens for the JSON escaping).
func TestInjectClaudeCodeSystemBlocksKeepsClientBlocks(t *testing.T) {
	body := []byte(`{"model":"m","system":[{"type":"text","text":"You are X.","cache_control":{"type":"ephemeral"}},{"type":"text","text":"<env>cwd=/tmp</env>"}],"messages":[{"role":"user","content":"hi"}]}`)
	out := injectClaudeCodeSystemBlocks(body)

	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 4 {
		t.Fatalf("want [billing, agent, 2 client blocks], got %d: %s", len(blocks), out)
	}
	if got := blocks[2].Get("text").String(); got != "You are X." {
		t.Errorf("client block came back mangled: %q", got)
	}
	if got := blocks[2].Get("cache_control.type").String(); got != "ephemeral" {
		t.Errorf("client's breakpoint destroyed: %s", out)
	}
	if got := blocks[3].Get("text").String(); got != "<env>cwd=/tmp</env>" {
		t.Errorf("second client block lost: %q", got)
	}
}

// A string system (what the OpenAI translation produces before injection) still
// has to work.
func TestInjectClaudeCodeSystemBlocksWrapsStringSystem(t *testing.T) {
	out := injectClaudeCodeSystemBlocks([]byte(`{"model":"m","system":"be brief","messages":[]}`))
	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 3 || blocks[2].Get("text").String() != "be brief" {
		t.Errorf("string system mishandled: %s", out)
	}
}

// Idempotence: the injection runs on both the Execute and stream paths.
func TestInjectClaudeCodeSystemBlocksIsIdempotent(t *testing.T) {
	body := []byte(`{"model":"m","system":[{"type":"text","text":"a"}],"messages":[]}`)
	once := injectClaudeCodeSystemBlocks(body)
	twice := injectClaudeCodeSystemBlocks(once)
	if string(once) != string(twice) {
		t.Errorf("second pass changed the body:\n%s\n%s", once, twice)
	}
}
