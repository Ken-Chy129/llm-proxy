package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

func relayAgainst(t *testing.T, handler http.HandlerFunc) *KimiExecutor {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Setenv("TEST_RELAY_STREAM_TOKEN", "relay-secret")
	exec := NewRelayExecutor(config.RelayConfig{
		Enabled:      true,
		BaseURL:      server.URL,
		AuthTokenEnv: "TEST_RELAY_STREAM_TOKEN",
	})
	exec.SetModels([]config.ModelConfig{{Name: "claude-opus-5"}})
	return exec
}

func streamRequest() *types.ChatCompletionRequest {
	return &types.ChatCompletionRequest{
		Model:     "claude-opus-5",
		MaxTokens: 16,
		Stream:    true,
		Messages:  []types.ChatMessage{{Role: "user", Content: []byte(`"hi"`)}},
	}
}

// An Anthropic-compatible upstream can accept the request with a 200 and only
// then report that it cannot serve it. Reporting success for such a stream is
// what let a relay failure reach the caller as a 500: the chain saw no error,
// so it never tried the next provider.
func TestRelayStreamSurfacesInBandUpstreamError(t *testing.T) {
	exec := relayAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"upstream is overloaded\"}}\n\n")
	})

	var out bytes.Buffer
	_, err := exec.ExecuteStream(context.Background(), streamRequest(), &out)
	if err == nil {
		t.Fatal("ExecuteStream() succeeded on a stream that only carried an error event")
	}
	if !strings.Contains(err.Error(), "upstream is overloaded") {
		t.Errorf("error = %q, want the upstream's own message so the dashboard can show it", err)
	}
	if got := StatusFromError(err); got != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 so the chain falls over to the next provider", got)
	}
}

// A stream that stops before message_delta cannot be turned into a valid
// Responses stream, so it has to fail inside the executor where failover is
// still possible.
func TestRelayStreamRejectsStreamWithoutStopReason(t *testing.T) {
	exec := relayAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4}}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n")
	})

	var out bytes.Buffer
	_, err := exec.ExecuteStream(context.Background(), streamRequest(), &out)
	if err == nil {
		t.Fatal("ExecuteStream() succeeded on a stream that never sent a stop reason")
	}
	if got := StatusFromError(err); got < 500 {
		t.Errorf("status = %d, want a 5xx so the chain treats it as an outage", got)
	}
	if strings.Contains(out.String(), "[DONE]") {
		t.Error("an incomplete stream must not be terminated with [DONE]")
	}
}

// The happy path must stay intact: a complete stream still terminates normally.
func TestRelayStreamStillCompletesNormally(t *testing.T) {
	exec := relayAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4}}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n")
	})

	var out bytes.Buffer
	usage, err := exec.ExecuteStream(context.Background(), streamRequest(), &out)
	if err != nil {
		t.Fatalf("ExecuteStream() on a complete stream = %v", err)
	}
	if !strings.Contains(out.String(), "\"finish_reason\":\"stop\"") {
		t.Errorf("translated stream = %q, want a finish_reason", out.String())
	}
	if !strings.Contains(out.String(), "[DONE]") {
		t.Error("a complete stream must be terminated with [DONE]")
	}
	if usage == nil {
		t.Fatal("usage = nil, want the accumulated token counts")
	}
}
