package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func turnStateBody(key string) []byte {
	b, _ := json.Marshal(map[string]interface{}{
		"model": "gpt-5.5", "stream": true, "prompt_cache_key": key,
		"input": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	return b
}

func openAndDrain(t *testing.T, exec *CodexExecutor, ctx context.Context, body []byte) {
	t.Helper()
	s, err := exec.OpenResponsesStream(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, s)
	_ = s.Close()
}

// The proxy fills in the last upstream turn-state when the client sends none,
// and never overrides a state the client did send.
func TestCodexRemembersTurnStateAcrossTurns(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	n := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("x-codex-turn-state"))
		n++
		if r.Header.Get("x-codex-turn-state") == "" { // upstream only mints when absent
			w.Header().Set("x-codex-turn-state", "up-"+strings.Repeat("x", n))
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completedResponsesStream)
	}))
	defer server.Close()
	withCodexUpstream(t, server.URL, server.URL)
	exec, _ := newCodexTestExecutor(t, "A")
	body := turnStateBody("thread-1")

	openAndDrain(t, exec, context.Background(), body) // no state yet
	openAndDrain(t, exec, context.Background(), body) // proxy fills "up-x"
	openAndDrain(t, exec, context.Background(), body) // upstream omitted header last turn; proxy still fills "up-x"
	h := http.Header{}
	h.Set("x-codex-turn-state", "client-own")
	openAndDrain(t, exec, WithClientHeaders(context.Background(), h), body) // client wins
	openAndDrain(t, exec, context.Background(), turnStateBody("thread-2"))  // other thread: none

	mu.Lock()
	defer mu.Unlock()
	want := []string{"", "up-x", "up-x", "client-own", ""}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("turn %d sent state %q, want %q (all=%v)", i, seen[i], want[i], seen)
		}
	}
}

// A state issued while account A served must not be replayed on account B,
// and a failed response must not overwrite the remembered state.
func TestCodexTurnStateIsAccountBoundAndSuccessOnly(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	limitA := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen = append(seen, tok+":"+r.Header.Get("x-codex-turn-state"))
		la := limitA
		mu.Unlock()
		if tok == "access-A" && la {
			w.Header().Set("x-codex-turn-state", "stale-from-429")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached"}}`)
			return
		}
		w.Header().Set("x-codex-turn-state", "state-"+tok)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completedResponsesStream)
	}))
	defer server.Close()
	withCodexUpstream(t, server.URL, server.URL)
	exec, _ := newCodexTestExecutor(t, "B", "A") // round-robin starts on A
	body := turnStateBody("thread-1")

	openAndDrain(t, exec, context.Background(), body) // A serves, remembers state-access-A
	mu.Lock()
	limitA = true
	mu.Unlock()
	openAndDrain(t, exec, context.Background(), body) // A 429s (or is skipped); B serves
	openAndDrain(t, exec, context.Background(), body) // B again: now carries B's own state

	mu.Lock()
	defer mu.Unlock()
	if seen[0] != "access-A:" {
		t.Fatalf("first turn = %q", seen[0])
	}
	sawBEmpty, sawBOwn := false, false
	for _, s := range seen[1:] {
		switch {
		case strings.HasPrefix(s, "access-A:") && s != "access-A:" && s != "access-A:state-access-A":
			t.Fatalf("A carried a foreign state: %q (all=%v)", s, seen)
		case s == "access-B:state-access-A" || s == "access-B:stale-from-429":
			t.Fatalf("B received A's or a failed attempt's state: %q (all=%v)", s, seen)
		case s == "access-B:":
			sawBEmpty = true
		case s == "access-B:state-access-B":
			sawBOwn = true
		}
	}
	if !sawBEmpty || !sawBOwn {
		t.Fatalf("expected B first without state then with its own; got %v", seen)
	}
}

// Round-robin over a two-account pool would alternate A/B and no turn would
// ever follow the one that issued its state. A conversation must keep going
// back to the account that last served it while that account is usable.
func TestCodexConversationStaysOnServingAccount(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens = append(tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		mu.Unlock()
		w.Header().Set("x-codex-turn-state", "s")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completedResponsesStream)
	}))
	defer server.Close()
	withCodexUpstream(t, server.URL, server.URL)
	exec, _ := newCodexTestExecutor(t, "B", "A")
	body := turnStateBody("thread-1")
	for i := 0; i < 4; i++ {
		openAndDrain(t, exec, context.Background(), body)
	}
	openAndDrain(t, exec, context.Background(), turnStateBody("thread-2")) // unrelated thread may land elsewhere

	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < 4; i++ {
		if tokens[i] != tokens[0] {
			t.Fatalf("turn %d moved to %s after %s served: %v", i, tokens[i], tokens[0], tokens)
		}
	}
}
