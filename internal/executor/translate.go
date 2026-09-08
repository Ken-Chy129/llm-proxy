package executor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Ken-Chy129/llm-proxy/internal/types"
	"github.com/google/uuid"
	"github.com/tidwall/sjson"
)

func ToAnthropicRequest(req *types.ChatCompletionRequest, model string) *types.AnthropicRequest {
	ar := &types.AnthropicRequest{
		Model:            model,
		MaxTokens:        req.MaxTokens,
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		Stream:           req.Stream,
		AnthropicVersion: "vertex-2023-10-16",
	}
	if ar.MaxTokens == 0 {
		ar.MaxTokens = 8192
	}

	var systemParts []string
	for _, msg := range req.Messages {
		if msg.Role == "system" || msg.Role == "developer" {
			systemParts = append(systemParts, extractText(msg.Content))
		}
	}
	if len(systemParts) > 0 {
		ar.System = []types.AnthropicSystemBlock{{
			Type: "text",
			Text: strings.Join(systemParts, "\n\n"),
		}}
	}

	for _, msg := range req.Messages {
		switch msg.Role {
		case "system", "developer":
			continue
		case "user":
			ar.Messages = append(ar.Messages, types.AnthropicMessage{
				Role:    "user",
				Content: toAnthropicContent(msg.Content),
			})
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				blocks := buildAssistantBlocks(msg)
				raw, _ := json.Marshal(blocks)
				ar.Messages = append(ar.Messages, types.AnthropicMessage{
					Role:    "assistant",
					Content: raw,
				})
			} else {
				ar.Messages = append(ar.Messages, types.AnthropicMessage{
					Role:    "assistant",
					Content: toAnthropicContent(msg.Content),
				})
			}
		case "tool":
			block := types.AnthropicContentBlock{
				Type:      "tool_result",
				ToolUseID: msg.ToolCallID,
				Content:   extractText(msg.Content),
			}
			raw, _ := json.Marshal([]types.AnthropicContentBlock{block})
			ar.Messages = append(ar.Messages, types.AnthropicMessage{
				Role:    "user",
				Content: raw,
			})
		}
	}

	for _, tool := range req.Tools {
		ar.Tools = append(ar.Tools, types.AnthropicTool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			InputSchema: tool.Function.Parameters,
		})
	}

	if stop := parseStop(req.Stop); len(stop) > 0 {
		ar.StopSequences = stop
	}

	dropTrailingAssistantPrefill(ar)

	return ar
}

// dropTrailingAssistantPrefill removes a trailing assistant turn that carries no
// tool calls.
//
// Anthropic reads a conversation ending in an assistant message as a *prefill*:
// "continue this text". Most models allow it, but several (Opus 4.6 and the
// other reasoning models among them) reject the request outright with
// "This model does not support assistant message prefill. The conversation must
// end with a user message."
//
// Clients on /v1/responses produce exactly this shape whenever a turn is
// interrupted — the aborted partial answer stays in the transcript and is
// replayed as the last input item on the retry. That made the failure look
// random: the same session would fail while a fresh query succeeded, because
// only the interrupted transcript ends this way.
//
// A prefill is dropped rather than padded with an empty user message: the text
// is a partial answer the model is about to produce again, so replaying it as
// context is at best redundant. An assistant turn holding tool_use blocks is
// left alone — it is not a prefill, and the tool_result that answers it is
// already carried as a user message after it.
func dropTrailingAssistantPrefill(ar *types.AnthropicRequest) {
	original := ar.Messages
	for len(ar.Messages) > 0 {
		last := ar.Messages[len(ar.Messages)-1]
		if last.Role != "assistant" || containsToolUse(last.Content) {
			return
		}
		ar.Messages = ar.Messages[:len(ar.Messages)-1]
	}
	// Every message was a prefill, and Anthropic rejects an empty conversation
	// too. Keep the original and let the upstream decide: an unusual request is
	// better than one we know is invalid.
	if len(original) > 0 {
		ar.Messages = original
	}
}

// containsToolUse reports whether a message's content holds a tool_use block,
// which makes an assistant turn part of a tool exchange rather than a prefill.
func containsToolUse(raw json.RawMessage) bool {
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "tool_use" {
			return true
		}
	}
	return false
}

// ApplyCacheBreakpoints marks prompt-caching breakpoints on a translated request.
//
// Anthropic caching is explicit: with no cache_control anywhere in the body the
// upstream treats every request as cold no matter how much of the prompt repeats.
// Nothing in the OpenAI wire format carries that marker, so every client arriving
// on /v1/* used to pay full price for an identical 100k-token prefix on every
// call — measured on this proxy: one such client burned 10.7M input tokens in a
// day at a 0% hit rate, while native /v1/messages traffic (whose client sets its
// own breakpoints) ran at 99.7%.
//
// This is deliberately NOT part of ToAnthropicRequest. That function also feeds
// Kimi's Anthropic-*compatible* endpoint, which is not Anthropic and has its own
// caching mechanism; sending it a field it does not model is a regression risk
// for no gain. Backends that speak real Anthropic (Claude OAuth, Vertex) opt in.
//
// Placement follows the order Anthropic concatenates the prompt in — tools, then
// system, then messages — since a breakpoint caches everything up to and
// including itself:
//
//   - last tool: the toolset is byte-identical across every request from a given
//     client, so this prefix is reused even between unrelated conversations.
//   - last system block: caches tools + system. Stable for a conversation's life.
//   - last message, but only when the request already contains an assistant turn.
//     A cache write costs 1.25x the tokens it covers, so marking a one-shot
//     request makes it *more* expensive to serve a prefix nobody will read back.
//     Agent loops, where each request is the previous one plus a turn, are
//     exactly where the write pays for itself.
//
// Anthropic allows at most 4 breakpoints; this uses at most 3.
func ApplyCacheBreakpoints(ar *types.AnthropicRequest) {
	if ar == nil {
		return
	}
	if n := len(ar.Tools); n > 0 {
		ar.Tools[n-1].CacheControl = types.Ephemeral()
	}
	if n := len(ar.System); n > 0 {
		ar.System[n-1].CacheControl = types.Ephemeral()
	}
	if n := len(ar.Messages); n > 0 && hasAssistantTurn(ar.Messages) {
		ar.Messages[n-1].Content = markLastContentBlock(ar.Messages[n-1].Content)
	}
}

// hasAssistantTurn reports whether this looks like a continuation rather than a
// fresh one-shot request.
func hasAssistantTurn(msgs []types.AnthropicMessage) bool {
	for _, m := range msgs {
		if m.Role == "assistant" {
			return true
		}
	}
	return false
}

// markLastContentBlock hangs a breakpoint on the final content block.
//
// Content is kept as raw JSON on purpose, so the marker is set with sjson rather
// than by unmarshalling into AnthropicContentBlock and back: that struct does not
// model every shape the API accepts (a tool_result whose content is an array of
// blocks, for one), and a round-trip would quietly drop whatever it cannot see.
func markLastContentBlock(raw json.RawMessage) json.RawMessage {
	// A plain string has nowhere to put the marker; promote it to one text block.
	var text string
	if json.Unmarshal(raw, &text) == nil {
		out, err := json.Marshal([]types.AnthropicContentBlock{{
			Type:         "text",
			Text:         text,
			CacheControl: types.Ephemeral(),
		}})
		if err != nil {
			return raw
		}
		return out
	}

	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil || len(blocks) == 0 {
		return raw
	}
	last, err := sjson.SetRawBytes(blocks[len(blocks)-1], "cache_control", []byte(`{"type":"ephemeral"}`))
	if err != nil {
		return raw
	}
	blocks[len(blocks)-1] = last
	out, err := json.Marshal(blocks)
	if err != nil {
		return raw
	}
	return out
}

func buildAssistantBlocks(msg types.ChatMessage) []types.AnthropicContentBlock {
	var blocks []types.AnthropicContentBlock
	text := extractText(msg.Content)
	if text != "" {
		blocks = append(blocks, types.AnthropicContentBlock{Type: "text", Text: text})
	}
	for _, tc := range msg.ToolCalls {
		blocks = append(blocks, types.AnthropicContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: toolUseInput(tc.Function.Arguments),
		})
	}
	return blocks
}

// toolUseInput normalises OpenAI tool-call arguments into something Anthropic
// will accept as a tool_use block's `input`.
//
// Anthropic requires the field on every tool_use block, but two shapes of
// OpenAI input break that: a parameterless tool call arrives with arguments
// "" (or "null"), which `json:"input,omitempty"` then strips from the body
// entirely, and a truncated argument stream arrives as invalid JSON, which
// makes json.Marshal fail for the whole message. Both surface as an upstream
// 400 ("tool_use.input: Field required"), so an empty object stands in — it is
// the accurate encoding of "this call takes no arguments".
func toolUseInput(arguments string) json.RawMessage {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" || trimmed == "null" || !json.Valid([]byte(trimmed)) {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(trimmed)
}

func FromAnthropicResponse(resp *types.AnthropicResponse, model string) *types.ChatCompletionResponse {
	result := &types.ChatResult{Role: "assistant"}
	var toolCalls []types.ToolCall

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			result.Content += block.Text
		case "tool_use":
			args, _ := json.Marshal(block.Input)
			toolCalls = append(toolCalls, types.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: types.ToolCallFunction{
					Name:      block.Name,
					Arguments: string(args),
				},
			})
		}
	}
	if len(toolCalls) > 0 {
		result.ToolCalls = toolCalls
	}

	finishReason := mapStopReason(resp.StopReason)

	usage := &types.Usage{}
	usage.SetBreakdown(resp.Usage.Breakdown())

	return &types.ChatCompletionResponse{
		ID:      fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:24]),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []types.ChatCompletionChoice{
			{Index: 0, Message: result, FinishReason: &finishReason},
		},
		Usage: usage,
	}
}

// toAnthropicContent normalises an OpenAI message content payload into a shape
// the Anthropic Messages API accepts.
//
// A plain string is passed through untouched. A multimodal array is converted
// block by block: "text" stays as-is, while OpenAI's "image_url" block is
// rewritten into Anthropic's "image" block with a source object. Without this
// the upstream rejects the request with:
//
//	Input tag 'image_url' found using 'type' does not match any of the expected tags
//
// Unknown block types are dropped rather than forwarded, since forwarding them
// would trigger the same validation error.
func toAnthropicContent(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}

	// Plain string content needs no translation.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return raw
	}

	var parts []openAIContentPart
	if json.Unmarshal(raw, &parts) != nil {
		// Not a recognised array shape — leave it alone.
		return raw
	}

	blocks := make([]types.AnthropicContentBlock, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text":
			blocks = append(blocks, types.AnthropicContentBlock{Type: "text", Text: p.Text})
		case "image_url", "input_image":
			url := contentPartImageURL(p)
			if url == "" {
				continue
			}
			if src := imageSourceFromURL(url); src != nil {
				blocks = append(blocks, types.AnthropicContentBlock{Type: "image", Source: src})
			}
		}
	}

	if len(blocks) == 0 {
		return raw
	}
	out, err := json.Marshal(blocks)
	if err != nil {
		return raw
	}
	return out
}

// openAIContentPart covers both Chat Completions ("image_url" as an object with
// a url field) and Responses-style ("input_image" with image_url as a bare
// string) multimodal blocks.
type openAIContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL json.RawMessage `json:"image_url,omitempty"`
}

// contentPartImageURL pulls the URL out of an image block, tolerating both the
// object form {"url": "..."} and the bare-string form "...".
func contentPartImageURL(p openAIContentPart) string {
	if len(p.ImageURL) == 0 {
		return ""
	}
	var obj struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(p.ImageURL, &obj) == nil && obj.URL != "" {
		return obj.URL
	}
	var s string
	if json.Unmarshal(p.ImageURL, &s) == nil {
		return s
	}
	return ""
}

// imageSourceFromURL builds an Anthropic image source from an OpenAI image URL,
// handling both data URIs (base64) and remote http(s) URLs.
func imageSourceFromURL(url string) *types.AnthropicMediaSource {
	if strings.HasPrefix(url, "data:") {
		meta, data, found := strings.Cut(strings.TrimPrefix(url, "data:"), ",")
		if !found || data == "" {
			return nil
		}
		mediaType := strings.TrimSuffix(meta, ";base64")
		if mediaType == meta {
			// Not base64-encoded — Anthropic only accepts base64 data payloads.
			return nil
		}
		if mediaType == "" {
			mediaType = "image/png"
		}
		return &types.AnthropicMediaSource{
			Type:      "base64",
			MediaType: mediaType,
			Data:      data,
		}
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return &types.AnthropicMediaSource{Type: "url", URL: url}
	}
	return nil
}

func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var texts []string
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

func parseStop(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		return arr
	}
	return nil
}

// anthropicStreamError decodes an Anthropic-style `error` SSE event. An
// Anthropic stream can open with 200 OK and then report the real failure
// (overloaded_error, invalid model, gateway trouble) as an in-band event, so a
// translator that only knows the happy-path event types would treat a failed
// stream as a successful empty one.
func anthropicStreamError(data string) (string, string, bool) {
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(data), &payload) != nil || payload.Type != "error" {
		return "", "", false
	}
	message := strings.TrimSpace(payload.Error.Message)
	if message == "" {
		message = strings.TrimSpace(data)
	}
	return strings.TrimSpace(payload.Error.Type), message, true
}

// anthropicStreamErrorStatus maps an in-band error type onto the HTTP status it
// would have carried had the upstream failed before sending headers. The status
// is what decides retryability in the chain: overload and internal faults are
// another provider's chance, while a bad request would fail identically
// everywhere.
func anthropicStreamErrorStatus(errType string) int {
	switch errType {
	case "overloaded_error":
		return http.StatusTooManyRequests
	case "rate_limit_error":
		return http.StatusTooManyRequests
	case "authentication_error", "permission_error":
		return http.StatusUnauthorized
	case "invalid_request_error", "not_found_error", "request_too_large":
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}

// incompleteStreamError describes a stream that ended without the terminal
// event carrying a stop reason. Callers translate this into a Responses stream,
// which cannot be terminated without one, so an incomplete stream has to be an
// error rather than a silently truncated success.
func incompleteStreamError(backend string, sawContent bool) error {
	detail := "no content and no stop reason"
	if sawContent {
		detail = "content arrived but the stop reason never did"
	}
	return &HTTPError{
		Backend: backend,
		Status:  http.StatusBadGateway,
		Body:    "upstream stream ended without a terminal message_delta: " + detail,
	}
}

func mapStopReason(reason string) string {
	switch reason {
	case "end_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	default:
		return "stop"
	}
}
