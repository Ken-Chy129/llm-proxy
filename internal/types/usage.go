package types

import "encoding/json"

// ReasoningUnknown marks a reasoning-token count the upstream never reported.
// Anthropic folds thinking tokens into output_tokens with no way to separate
// them, so a Claude-served request genuinely has no answer here — which is a
// different fact from "it did no thinking". The UI renders this as "—".
const ReasoningUnknown = -1

// TokenUsage is the canonical accounting used for storage and display: four
// non-overlapping buckets that can be summed without double-counting.
//
// This deliberately differs from both upstream wire formats:
//   - Anthropic reports input_tokens already excluding cache, in separate fields
//   - OpenAI reports prompt_tokens *including* cached, with cached as a subset
//
// Breakdown() on each wire type is the only place that conversion happens.
type TokenUsage struct {
	Input      int // input tokens that were neither read from nor written to a cache
	CacheRead  int // input tokens served from a cache hit
	CacheWrite int // input tokens charged for creating a cache entry
	Output     int // all output tokens, thinking/reasoning included
	Reasoning  int // the reasoning subset of Output, or ReasoningUnknown
}

// Total is the display figure: every token the request moved. Reasoning is
// excluded because it is a subset of Output, not a fifth bucket.
func (u TokenUsage) Total() int {
	return u.Input + u.CacheRead + u.CacheWrite + u.Output
}

// Breakdown converts Anthropic's usage object. Anthropic's input_tokens is
// already cache-exclusive, so the fields map straight across.
func (u AnthropicUsage) Breakdown() TokenUsage {
	return TokenUsage{
		Input:      u.InputTokens,
		CacheRead:  u.CacheReadInputTokens,
		CacheWrite: u.CacheCreationInputTokens,
		Output:     u.OutputTokens,
		Reasoning:  ReasoningUnknown,
	}
}

// ResponsesUsage mirrors the OpenAI Responses API usage object, which nests its
// cache and reasoning counts one level deeper than Chat Completions does.
// input_tokens is cache-inclusive; output_tokens is reasoning-inclusive.
type ResponsesUsage struct {
	InputTokens         int                   `json:"input_tokens"`
	OutputTokens        int                   `json:"output_tokens"`
	InputTokensDetails  *ResponsesInputDetail `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *ResponsesOutputDetail `json:"output_tokens_details,omitempty"`
}

type ResponsesInputDetail struct {
	CachedTokens int `json:"cached_tokens"`
}

type ResponsesOutputDetail struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

func (u ResponsesUsage) Breakdown() TokenUsage {
	b := TokenUsage{
		Input:     u.InputTokens,
		Output:    u.OutputTokens,
		Reasoning: ReasoningUnknown,
	}
	if d := u.InputTokensDetails; d != nil {
		b.CacheRead = d.CachedTokens
		b.Input -= d.CachedTokens
		if b.Input < 0 {
			b.Input = 0
		}
	}
	if d := u.OutputTokensDetails; d != nil {
		b.Reasoning = d.ReasoningTokens
	}
	return b
}

// ParseResponsesUsage pulls the usage object out of a decoded
// `response.completed` event body. Returns nil when the event carries none.
func ParseResponsesUsage(event map[string]interface{}) *ResponsesUsage {
	resp, ok := event["response"].(map[string]interface{})
	if !ok {
		return nil
	}
	raw, ok := resp["usage"]
	if !ok || raw == nil {
		return nil
	}
	// Re-marshal rather than hand-walking the nested detail objects: one extra
	// round-trip per request buys typed parsing that can't silently miss a field.
	blob, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var u ResponsesUsage
	if json.Unmarshal(blob, &u) != nil {
		return nil
	}
	return &u
}

// Breakdown converts an OpenAI-shaped usage object. prompt_tokens there is
// cache-inclusive, so the cached subset has to be subtracted out to get a
// bucket that can be added to CacheRead without counting it twice. The clamp
// guards against an upstream reporting cached_tokens > prompt_tokens.
//
// OpenAI-style automatic caching has no separate write charge, so CacheWrite
// only ever comes from the Anthropic extension field.
func (u Usage) Breakdown() TokenUsage {
	b := TokenUsage{
		Input:     u.PromptTokens,
		Output:    u.CompletionTokens,
		Reasoning: ReasoningUnknown,
	}
	if d := u.PromptTokensDetails; d != nil {
		b.CacheRead = d.CachedTokens
		b.CacheWrite = d.CacheWriteTokens
		// Both cache buckets are subsets of prompt_tokens, so both come out.
		// Subtracting only the read half would leave cache-write counted twice —
		// once inside Input and once as CacheWrite — inflating every total.
		b.Input -= d.CachedTokens + d.CacheWriteTokens
		if b.Input < 0 {
			b.Input = 0
		}
	}
	if d := u.CompletionTokensDetails; d != nil {
		b.Reasoning = d.ReasoningTokens
	}
	return b
}
