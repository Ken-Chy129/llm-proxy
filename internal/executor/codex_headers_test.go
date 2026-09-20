package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The Codex client sends session/routing headers and replays the turn-state
// upstream returned on the previous turn. The proxy must forward the former
// and surface the latter, while never leaking its own auth or identity
// headers' control to the client.
func TestCodexForwardsSessionHeadersAndRecordsTurnState(t *testing.T) {
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("x-codex-turn-state", "state-from-upstream")
		w.Header().Set("x-request-id", "req-42")
		w.Header().Set("Set-Cookie", "must-not-leak")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completedResponsesStream)
	}))
	defer server.Close()
	withCodexUpstream(t, server.URL, server.URL)
	exec, _ := newCodexTestExecutor(t, "A")

	client := http.Header{}
	client.Set("x-codex-turn-state", "state-from-client")
	client.Set("x-codex-routing-hint", "hint")
	client.Set("session_id", "sess-1")
	client.Set("chatgpt-account-id", "acct-1")
	client.Set("Authorization", "Bearer client-token") // never forwarded
	client.Set("User-Agent", "Codex Desktop/1.0")      // proxy owns UA
	client.Set("x-codex-installation-id", "client-install")
	client.Set("x-client-request-id", "client-req")

	ctx := WithClientHeaders(context.Background(), client)
	ctx, upstream := WithUpstreamHeaderRecorder(ctx)
	stream, err := exec.OpenResponsesStream(ctx, encryptedSessionBody(""))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, stream)
	_ = stream.Close()

	for name, want := range map[string]string{
		"x-codex-turn-state":   "state-from-client",
		"x-codex-routing-hint": "hint",
		"session_id":           "sess-1",
		"chatgpt-account-id":   "acct-1",
	} {
		if got := seen.Get(name); got != want {
			t.Errorf("upstream %s = %q, want %q", name, got, want)
		}
	}
	if got := seen.Get("Authorization"); got != "Bearer access-A" {
		t.Errorf("Authorization = %q, want the account token", got)
	}
	if got := seen.Get("User-Agent"); got != codexUserAgent {
		t.Errorf("User-Agent = %q, want proxy UA", got)
	}
	if got := seen.Get("x-codex-installation-id"); got == "client-install" {
		t.Error("client installation id must not override the proxy's stable id")
	}
	if got := seen.Get("x-client-request-id"); got == "client-req" {
		t.Error("proxy-minted request id must take precedence")
	}

	got := upstream()
	if got.Get("x-codex-turn-state") != "state-from-upstream" || got.Get("x-request-id") != "req-42" {
		t.Fatalf("recorded upstream headers = %v", got)
	}
	if got.Get("Set-Cookie") != "" {
		t.Fatal("non-allowlisted upstream header leaked")
	}
}

// A failed first attempt must not leave another account's turn-state behind.
func TestCodexTurnStateReflectsServingAttempt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer access-A" {
			w.Header().Set("x-codex-turn-state", "stale-A")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached"}}`)
			return
		}
		w.Header().Set("x-codex-turn-state", "fresh-B")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completedResponsesStream)
	}))
	defer server.Close()
	withCodexUpstream(t, server.URL, server.URL)
	exec, _ := newCodexTestExecutor(t, "B", "A")

	ctx, upstream := WithUpstreamHeaderRecorder(context.Background())
	stream, err := exec.OpenResponsesStream(ctx, encryptedSessionBody(""))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, stream)
	_ = stream.Close()
	if got := upstream().Get("x-codex-turn-state"); got != "fresh-B" {
		t.Fatalf("turn-state = %q, want the serving attempt's value", got)
	}
}
