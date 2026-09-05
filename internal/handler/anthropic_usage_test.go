package handler

import (
	"strings"
	"testing"
)

// The /v1/messages passthrough is the Claude Code path, and it is where the
// under-reporting was worst: cache tokens were parsed by nothing at all, so a
// 900K-token cached turn was logged as the ~12K that missed the cache.
func TestCopyStreamAndExtractUsageCapturesCacheBuckets(t *testing.T) {
	// A realistic cached turn: message_start carries input + both cache buckets,
	// message_delta carries output.
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1200,"cache_creation_input_tokens":3000,"cache_read_input_tokens":95000,"output_tokens":1}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":800}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var out strings.Builder
	usage, err := copyStreamAndExtractUsage(strings.NewReader(stream), &out)
	if err != nil {
		t.Fatalf("copyStreamAndExtractUsage: %v", err)
	}

	if usage.InputTokens != 1200 {
		t.Errorf("input = %d, want 1200", usage.InputTokens)
	}
	if usage.CacheReadInputTokens != 95000 {
		t.Errorf("cache read = %d, want 95000 — the whole point of this change", usage.CacheReadInputTokens)
	}
	if usage.CacheCreationInputTokens != 3000 {
		t.Errorf("cache write = %d, want 3000", usage.CacheCreationInputTokens)
	}
	if usage.OutputTokens != 800 {
		t.Errorf("output = %d, want 800 (message_delta must win over message_start's placeholder)", usage.OutputTokens)
	}

	b := usage.Breakdown()
	if want := 100000; b.Total() != want {
		t.Errorf("total = %d, want %d", b.Total(), want)
	}

	// The stream must reach the client byte-intact; extraction is a side effect.
	if !strings.Contains(out.String(), `"text":"hi"`) || !strings.Contains(out.String(), "message_stop") {
		t.Error("forwarded stream lost content")
	}
}

// Newer API versions repeat the full usage object in message_delta with the
// input fields zeroed. A last-write-wins merge silently erases the cache counts.
func TestCopyStreamAndExtractUsageDeltaDoesNotEraseCache(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":1200,"cache_read_input_tokens":95000,"cache_creation_input_tokens":3000}}}`,
		``,
		`data: {"type":"message_delta","usage":{"input_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":800}}`,
		``,
	}, "\n")

	var out strings.Builder
	usage, err := copyStreamAndExtractUsage(strings.NewReader(stream), &out)
	if err != nil {
		t.Fatalf("copyStreamAndExtractUsage: %v", err)
	}
	if usage.CacheReadInputTokens != 95000 || usage.CacheCreationInputTokens != 3000 || usage.InputTokens != 1200 {
		t.Errorf("delta's zeroes clobbered message_start: %+v", *usage)
	}
	if usage.OutputTokens != 800 {
		t.Errorf("output = %d, want 800", usage.OutputTokens)
	}
}

func TestExtractUsageFromResponseReadsCache(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":500,"cache_creation_input_tokens":30}}`)
	u := extractUsageFromResponse(body)
	if u == nil {
		t.Fatal("got nil usage")
	}
	if u.CacheReadInputTokens != 500 || u.CacheCreationInputTokens != 30 {
		t.Errorf("got %+v, want cache read 500 / write 30", *u)
	}
	if extractUsageFromResponse([]byte(`{"id":"msg_1"}`)) != nil {
		t.Error("a body with no usage object must yield nil, not a zero-value usage")
	}
}

// The /v1/responses passthrough logged zero tokens for every Codex CLI request
// before this extractor existed.
func TestCopyResponsesStreamExtractsUsage(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"ok"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":9000,"output_tokens":4000,"input_tokens_details":{"cached_tokens":7500},"output_tokens_details":{"reasoning_tokens":1300}}}}`,
		``,
	}, "\n")

	var out strings.Builder
	usage, err := copyResponsesStreamAndExtractUsage(strings.NewReader(stream), &out)
	if err != nil {
		t.Fatalf("copyResponsesStreamAndExtractUsage: %v", err)
	}
	if usage == nil {
		t.Fatal("got nil usage — Codex traffic would log as zero tokens")
	}
	b := usage.Breakdown()
	if b.Input != 1500 || b.CacheRead != 7500 || b.Output != 4000 || b.Reasoning != 1300 {
		t.Errorf("got %+v, want input 1500 / read 7500 / output 4000 / reasoning 1300", b)
	}
	if !strings.Contains(out.String(), `"delta":"ok"`) {
		t.Error("forwarded stream lost content")
	}
}

func TestCopyIncompleteResponsesStreamExtractsUsage(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.incomplete`,
		`data: {"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":9000,"output_tokens":4000}}}`,
		``,
	}, "\n")

	var out strings.Builder
	usage, err := copyResponsesStreamAndExtractUsage(strings.NewReader(stream), &out)
	if err != nil {
		t.Fatalf("copyResponsesStreamAndExtractUsage: %v", err)
	}
	if usage == nil || usage.InputTokens != 9000 || usage.OutputTokens != 4000 {
		t.Fatalf("usage = %+v, want incomplete response usage", usage)
	}
}

func TestCopyResponsesStreamWithoutCompletedEvent(t *testing.T) {
	// A stream that dies before response.completed has no usage to report; the
	// caller must get nil rather than a zero-value object claiming 0 tokens.
	var out strings.Builder
	usage, err := copyResponsesStreamAndExtractUsage(
		strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"), &out)
	if err != nil {
		t.Fatalf("copyResponsesStreamAndExtractUsage: %v", err)
	}
	if usage != nil {
		t.Errorf("got %+v, want nil", usage)
	}
}
