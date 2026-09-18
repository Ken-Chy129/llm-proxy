package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ken-Chy129/llm-proxy/internal/auth"
)

// Once every account is cooling down after a 429, a new request must not be
// sent upstream at all: the old "always try something" selection handed each
// limited account back in turn, so every request against a spent pool collected
// one real 429 per account before the chain failed over. The executor now
// reports a 429 immediately, without touching the upstream, so the chain moves
// to the next provider.
func TestCodexAllAccountsLimitedFailsFastWith429(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":1455}}`))
	}))
	defer upstream.Close()
	withCodexUpstream(t, upstream.URL, upstream.URL+"/token")

	exec, store := newCodexTestExecutor(t, "A", "B", "C")
	for _, id := range []string{"A", "B", "C"} {
		store.MarkRateLimited("codex", id, "", time.Now().Add(20*time.Minute), false)
	}

	for name, call := range map[string]func() error{
		"responses": func() error {
			_, err := exec.OpenResponsesStream(context.Background(), []byte(`{"model":"gpt-5.5","input":"hi"}`))
			return err
		},
		"chat": func() error {
			_, err := exec.ExecuteStream(context.Background(), testChatRequest(), &strings.Builder{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			hits.Store(0)
			err := call()
			if err == nil {
				t.Fatal("expected an error while every account is limited")
			}
			if got := StatusFromError(err); got != http.StatusTooManyRequests {
				t.Fatalf("status = %d (%v), want 429 so the chain fails over", got, err)
			}
			if n := hits.Load(); n != 0 {
				t.Fatalf("upstream was called %d time(s); a fully limited pool must not be retried", n)
			}
		})
	}

	// Quota alone (no reactive cooldown) must produce the same short-circuit.
	store.ClearRateLimit("codex", "A")
	store.ClearRateLimit("codex", "B")
	store.ClearRateLimit("codex", "C")
	for _, id := range []string{"A", "B", "C"} {
		auth.QuotaCache.Set("codex:"+id, &auth.QuotaInfo{AccountID: id, HasRealData: true,
			Primary: &auth.RateWindow{LimitReached: true, ResetUnix: time.Now().Add(time.Hour).Unix()}})
	}
	hits.Store(0)
	_, err := exec.OpenResponsesStream(context.Background(), []byte(`{"model":"gpt-5.5","input":"hi"}`))
	if StatusFromError(err) != http.StatusTooManyRequests || hits.Load() != 0 {
		t.Fatalf("quota-exhausted pool: err=%v hits=%d, want 429 and no upstream calls", err, hits.Load())
	}

	// Once one window's reset passes, that account is tried again.
	auth.QuotaCache.Set("codex:B", &auth.QuotaInfo{AccountID: "B", HasRealData: true,
		Primary: &auth.RateWindow{LimitReached: true, ResetUnix: time.Now().Add(-time.Minute).Unix()}})
	_, _ = exec.OpenResponsesStream(context.Background(), []byte(`{"model":"gpt-5.5","input":"hi"}`))
	if hits.Load() != 1 {
		t.Fatalf("expected exactly one upstream attempt on the recovered account, got %d", hits.Load())
	}
}
