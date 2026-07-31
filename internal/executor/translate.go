package executor

import (
	"encoding/json"
	"fmt"
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

	return ar
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
			Input: json.RawMessage(tc.Function.Arguments),
		})
	}
	return blocks
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
