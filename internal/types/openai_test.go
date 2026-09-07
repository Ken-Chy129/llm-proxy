package types

import (
	"strings"
	"testing"
)

// EmptyOutputReason is what turns an opaque "no usable assistant output" 502
// into something actionable, so each distinguishable upstream failure mode
// needs to stay distinguishable.
func TestEmptyOutputReasonExplainsEachFailureMode(t *testing.T) {
	stop := "stop"
	length := "length"
	filtered := "content_filter"

	tests := []struct {
		name string
		resp *ChatCompletionResponse
		want string
	}{
		{
			name: "nil response",
			resp: nil,
			want: "response body was empty",
		},
		{
			name: "no choices",
			resp: &ChatCompletionResponse{},
			want: "zero choices",
		},
		{
			name: "missing message",
			resp: &ChatCompletionResponse{
				Choices: []ChatCompletionChoice{{Index: 0}},
			},
			want: "no message",
		},
		{
			name: "blank content",
			resp: &ChatCompletionResponse{
				Choices: []ChatCompletionChoice{{
					Message:      &ChatResult{Role: "assistant", Content: "   "},
					FinishReason: &stop,
				}},
			},
			want: "finish_reason=stop",
		},
		{
			name: "reasoning only",
			resp: &ChatCompletionResponse{
				Choices: []ChatCompletionChoice{{
					Message:      &ChatResult{Role: "assistant", ReasoningContent: "thinking..."},
					FinishReason: &length,
				}},
			},
			want: "only reasoning_content",
		},
		{
			name: "content filter",
			resp: &ChatCompletionResponse{
				Choices: []ChatCompletionChoice{{
					Message:      &ChatResult{Role: "assistant"},
					FinishReason: &filtered,
				}},
			},
			want: "finish_reason=content_filter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.resp.EmptyOutputReason()
			if !strings.Contains(got, tt.want) {
				t.Fatalf("EmptyOutputReason() = %q, want it to mention %q", got, tt.want)
			}
		})
	}
}

func TestEmptyOutputReasonSummarisesMultipleChoices(t *testing.T) {
	stop := "stop"
	resp := &ChatCompletionResponse{
		Choices: []ChatCompletionChoice{
			{Message: &ChatResult{Role: "assistant"}, FinishReason: &stop},
			{Message: nil},
		},
	}
	got := resp.EmptyOutputReason()
	if !strings.Contains(got, "all 2 choices unusable") {
		t.Fatalf("EmptyOutputReason() = %q, want a multi-choice summary", got)
	}
}

// A usable response must report no reason at all, so callers can treat
// EmptyOutputReason() == "" as the single source of truth.
func TestEmptyOutputReasonSilentWhenOutputUsable(t *testing.T) {
	stop := "stop"
	usable := []*ChatCompletionResponse{
		{Choices: []ChatCompletionChoice{{
			Message:      &ChatResult{Role: "assistant", Content: "hello"},
			FinishReason: &stop,
		}}},
		{Choices: []ChatCompletionChoice{{
			Message: &ChatResult{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Type: "function"}}},
		}}},
		// One good choice is enough even when another is empty.
		{Choices: []ChatCompletionChoice{
			{Message: &ChatResult{Role: "assistant"}},
			{Message: &ChatResult{Role: "assistant", Content: "hi"}},
		}},
	}

	for i, resp := range usable {
		if got := resp.EmptyOutputReason(); got != "" {
			t.Fatalf("case %d: EmptyOutputReason() = %q, want empty", i, got)
		}
	}
}

// EmptyOutputIsDeterministic decides whether the chain retries, so the split
// between "would fail everywhere" and "this upstream misbehaved" is load-bearing.
func TestEmptyOutputIsDeterministic(t *testing.T) {
	stop := "stop"
	length := "length"
	filtered := "content_filter"
	blank := ""

	tests := []struct {
		name string
		resp *ChatCompletionResponse
		want bool
	}{
		{
			name: "truncated before any text",
			resp: &ChatCompletionResponse{Choices: []ChatCompletionChoice{{
				Message:      &ChatResult{Role: "assistant"},
				FinishReason: &length,
			}}},
			want: true,
		},
		{
			name: "content filtered",
			resp: &ChatCompletionResponse{Choices: []ChatCompletionChoice{{
				Message:      &ChatResult{Role: "assistant"},
				FinishReason: &filtered,
			}}},
			want: true,
		},
		{
			name: "reasoning only without finish reason",
			resp: &ChatCompletionResponse{Choices: []ChatCompletionChoice{{
				Message: &ChatResult{Role: "assistant", ReasoningContent: "thinking"},
			}}},
			want: true,
		},
		{
			name: "stop with empty content may differ on retry",
			resp: &ChatCompletionResponse{Choices: []ChatCompletionChoice{{
				Message:      &ChatResult{Role: "assistant"},
				FinishReason: &stop,
			}}},
			want: false,
		},
		{
			name: "blank finish reason may differ on retry",
			resp: &ChatCompletionResponse{Choices: []ChatCompletionChoice{{
				Message:      &ChatResult{Role: "assistant"},
				FinishReason: &blank,
			}}},
			want: false,
		},
		{
			name: "zero choices is a broken upstream",
			resp: &ChatCompletionResponse{},
			want: false,
		},
		{
			name: "missing message is a broken upstream",
			resp: &ChatCompletionResponse{Choices: []ChatCompletionChoice{{Index: 0}}},
			want: false,
		},
		{
			name: "nil response",
			resp: nil,
			want: false,
		},
		{
			name: "usable output is never deterministic emptiness",
			resp: &ChatCompletionResponse{Choices: []ChatCompletionChoice{{
				Message:      &ChatResult{Role: "assistant", Content: "hello"},
				FinishReason: &length,
			}}},
			want: false,
		},
		{
			name: "one retryable choice makes the whole response retryable",
			resp: &ChatCompletionResponse{Choices: []ChatCompletionChoice{
				{Message: &ChatResult{Role: "assistant"}, FinishReason: &length},
				{Message: &ChatResult{Role: "assistant"}, FinishReason: &stop},
			}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.resp.EmptyOutputIsDeterministic(); got != tt.want {
				t.Fatalf("EmptyOutputIsDeterministic() = %v, want %v", got, tt.want)
			}
		})
	}
}
