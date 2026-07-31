package types

import "testing"

// The two upstream families disagree about what "input tokens" means, and that
// disagreement is the whole reason Breakdown exists. These tests pin the
// conversion in both directions.

func TestAnthropicBreakdownKeepsBucketsDisjoint(t *testing.T) {
	// Anthropic's input_tokens already excludes both cache buckets, so the three
	// input fields add up to the real prompt size.
	u := AnthropicUsage{
		InputTokens:              1200,
		OutputTokens:             800,
		CacheReadInputTokens:     95000,
		CacheCreationInputTokens: 3000,
	}
	b := u.Breakdown()
	if b.Input != 1200 || b.CacheRead != 95000 || b.CacheWrite != 3000 || b.Output != 800 {
		t.Errorf("got %+v, want input 1200 / read 95000 / write 3000 / output 800", b)
	}
	if want := 100000; b.Total() != want {
		t.Errorf("Total() = %d, want %d", b.Total(), want)
	}
	if b.Reasoning != ReasoningUnknown {
		t.Errorf("Reasoning = %d, want ReasoningUnknown — Anthropic folds thinking into output", b.Reasoning)
	}
}

func TestOpenAIBreakdownSubtractsCachedFromPrompt(t *testing.T) {
	// OpenAI's prompt_tokens INCLUDES cached_tokens. Failing to subtract would
	// count the cache twice in the total.
	u := Usage{
		PromptTokens:            10000,
		CompletionTokens:        2000,
		PromptTokensDetails:     &PromptTokensDetails{CachedTokens: 8000},
		CompletionTokensDetails: &CompletionTokensDetails{ReasoningTokens: 750},
	}
	b := u.Breakdown()
	if b.Input != 2000 {
		t.Errorf("Input = %d, want 2000 (10000 prompt - 8000 cached)", b.Input)
	}
	if b.CacheRead != 8000 || b.CacheWrite != 0 {
		t.Errorf("cache = read %d / write %d, want 8000/0", b.CacheRead, b.CacheWrite)
	}
	if b.Reasoning != 750 {
		t.Errorf("Reasoning = %d, want 750", b.Reasoning)
	}
	if want := 12000; b.Total() != want {
		t.Errorf("Total() = %d, want %d (cache must not be double-counted)", b.Total(), want)
	}
}

func TestOpenAIBreakdownWithoutDetails(t *testing.T) {
	// No details object: nothing is cached, and reasoning is unknown rather than
	// zero — an upstream that doesn't report it hasn't told us it was zero.
	b := Usage{PromptTokens: 500, CompletionTokens: 100}.Breakdown()
	if b.Input != 500 || b.CacheRead != 0 || b.Output != 100 {
		t.Errorf("got %+v, want input 500 / read 0 / output 100", b)
	}
	if b.Reasoning != ReasoningUnknown {
		t.Errorf("Reasoning = %d, want ReasoningUnknown", b.Reasoning)
	}
}

func TestOpenAIBreakdownClampsBogusCachedCount(t *testing.T) {
	// Defensive: an upstream reporting cached > prompt must not produce negative
	// input, which would silently shrink every aggregate it lands in.
	b := Usage{
		PromptTokens:        100,
		PromptTokensDetails: &PromptTokensDetails{CachedTokens: 400},
	}.Breakdown()
	if b.Input != 0 {
		t.Errorf("Input = %d, want 0 (must clamp, not go negative)", b.Input)
	}
}

func TestResponsesBreakdownReadsNestedDetails(t *testing.T) {
	u := ResponsesUsage{
		InputTokens:         9000,
		OutputTokens:        4000,
		InputTokensDetails:  &ResponsesInputDetail{CachedTokens: 7500},
		OutputTokensDetails: &ResponsesOutputDetail{ReasoningTokens: 1300},
	}
	b := u.Breakdown()
	if b.Input != 1500 || b.CacheRead != 7500 || b.Output != 4000 || b.Reasoning != 1300 {
		t.Errorf("got %+v, want input 1500 / read 7500 / output 4000 / reasoning 1300", b)
	}
}

func TestSetBreakdownRoundTrips(t *testing.T) {
	// Anthropic-reading executors return an OpenAI-shaped usage object, and the
	// stats layer converts it back. A lossy round-trip here would mean cache
	// tokens vanish somewhere between the upstream and the database.
	in := TokenUsage{Input: 1200, CacheRead: 95000, CacheWrite: 3000, Output: 800, Reasoning: ReasoningUnknown}
	var wire Usage
	wire.SetBreakdown(in)

	if want := 99200; wire.PromptTokens != want {
		t.Errorf("wire prompt_tokens = %d, want %d (OpenAI semantics: cache-inclusive)", wire.PromptTokens, want)
	}
	if got := wire.Breakdown(); got != in {
		t.Errorf("round trip gave %+v, want %+v", got, in)
	}
}

func TestSetBreakdownOmitsDetailsWhenNothingToReport(t *testing.T) {
	// A plain uncached, non-reasoning request must serialise exactly as it did
	// before these fields existed, or every OpenAI client sees a shape change.
	var wire Usage
	wire.SetBreakdown(TokenUsage{Input: 500, Output: 100, Reasoning: ReasoningUnknown})
	if wire.PromptTokensDetails != nil {
		t.Errorf("PromptTokensDetails = %+v, want nil", wire.PromptTokensDetails)
	}
	if wire.CompletionTokensDetails != nil {
		t.Errorf("CompletionTokensDetails = %+v, want nil", wire.CompletionTokensDetails)
	}
}

// TestMergeNonZeroKeepsCacheAcrossEvents is the regression guard for the bug
// that motivated this work: message_delta repeats the usage object with the
// input fields zeroed, and a plain assignment there wipes out the cache counts
// captured from message_start.
func TestMergeNonZeroKeepsCacheAcrossEvents(t *testing.T) {
	var u AnthropicUsage
	u.MergeNonZero(AnthropicUsage{InputTokens: 1200, CacheReadInputTokens: 95000, CacheCreationInputTokens: 3000})
	u.MergeNonZero(AnthropicUsage{OutputTokens: 800}) // the delta, input fields zeroed

	if u.CacheReadInputTokens != 95000 || u.CacheCreationInputTokens != 3000 {
		t.Errorf("cache lost across events: %+v", u)
	}
	if u.InputTokens != 1200 || u.OutputTokens != 800 {
		t.Errorf("got input %d / output %d, want 1200/800", u.InputTokens, u.OutputTokens)
	}
}

func TestParseResponsesUsageHandlesMissingPieces(t *testing.T) {
	for name, event := range map[string]map[string]interface{}{
		"no response key": {"type": "response.completed"},
		"no usage key":    {"response": map[string]interface{}{"id": "resp_1"}},
		"null usage":      {"response": map[string]interface{}{"usage": nil}},
	} {
		if got := ParseResponsesUsage(event); got != nil {
			t.Errorf("%s: got %+v, want nil", name, got)
		}
	}

	event := map[string]interface{}{"response": map[string]interface{}{
		"usage": map[string]interface{}{
			"input_tokens":          float64(9000),
			"output_tokens":         float64(4000),
			"input_tokens_details":  map[string]interface{}{"cached_tokens": float64(7500)},
			"output_tokens_details": map[string]interface{}{"reasoning_tokens": float64(1300)},
		},
	}}
	u := ParseResponsesUsage(event)
	if u == nil {
		t.Fatal("got nil, want parsed usage")
	}
	if b := u.Breakdown(); b.Input != 1500 || b.CacheRead != 7500 || b.Reasoning != 1300 {
		t.Errorf("got %+v, want input 1500 / read 7500 / reasoning 1300", b)
	}
}
