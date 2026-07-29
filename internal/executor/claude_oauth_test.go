package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Ken-Chy129/llm-proxy/internal/auth"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// A single 429 must never sideline the whole Claude account for longer than
// maxClaudeReactiveCooldown. Anthropic reports the weekly boundary as the reset
// even when a model-specific cap (e.g. Fable/Opus weekly) was hit, so a far
// reset gets clamped and its "known" reset flag dropped. Short, legit resets
// (e.g. a Retry-After of a few seconds) pass through untouched.
func TestCapReactiveCooldown(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)

	// Weekly-boundary reset (~6 days out) is clamped to now + max, known→false.
	weekly := now.Add(6 * 24 * time.Hour)
	got, known := capReactiveCooldown(weekly, true, now)
	if want := now.Add(maxClaudeReactiveCooldown); !got.Equal(want) {
		t.Errorf("far reset: until=%v want %v", got, want)
	}
	if known {
		t.Errorf("far reset: known must be forced to false")
	}

	// A short, authoritative reset within the cap is left as-is.
	short := now.Add(30 * time.Second)
	got, known = capReactiveCooldown(short, true, now)
	if !got.Equal(short) || !known {
		t.Errorf("short reset changed: until=%v known=%v", got, known)
	}

	// The default-cooldown case (estimated, within cap) is preserved too.
	def := now.Add(60 * time.Second)
	got, known = capReactiveCooldown(def, false, now)
	if !got.Equal(def) || known {
		t.Errorf("default cooldown changed: until=%v known=%v", got, known)
	}
}

func TestClaudeExtraUsage400FailsOverToAnotherAccount(t *testing.T) {
	dir := t.TempDir()
	store := auth.NewTokenStore(dir, auth.StrategyRoundRobin)
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	for _, id := range []string{"A", "B"} {
		if err := store.Add(&auth.TokenData{ID: id, Provider: "claude", AccessToken: "token-" + id, ExpiresAt: future}); err != nil {
			t.Fatalf("add account %s: %v", id, err)
		}
	}

	executor := NewClaudeOAuthExecutor(auth.NewClaudeOAuth(store), nil)
	var tokens []string
	executor.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		tokens = append(tokens, strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
		status := http.StatusBadRequest
		body := `{"type":"error","error":{"type":"invalid_request_error","message":"You're out of extra usage. Ask your workspace admin to add more so you can keep going."}}`
		if len(tokens) == 2 {
			status = http.StatusOK
			body = `{"ok":true}`
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}

	ctx, accountResult := WithAccountRecorder(context.Background())
	resp, err := executor.doWithFailover(ctx, "claude-opus-5", func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test/messages", nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return req, err
	})
	if err != nil {
		t.Fatalf("doWithFailover: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want %d", resp.StatusCode, http.StatusOK)
	}
	if len(tokens) != 2 || tokens[0] == tokens[1] {
		t.Fatalf("expected two different Claude accounts, got %v", tokens)
	}

	account, failedOver := accountResult()
	if account == "" || len(failedOver) != 1 || failedOver[0] == account {
		t.Fatalf("account=%q failedOver=%v", account, failedOver)
	}
	if _, _, active := store.RateLimitInfo("claude", failedOver[0]); !active {
		t.Fatalf("extra-usage account %q was not put on account-wide cooldown", failedOver[0])
	}
}

func TestClaudeExtraUsage400PreservesFinalResponseBody(t *testing.T) {
	dir := t.TempDir()
	store := auth.NewTokenStore(dir, auth.StrategyRoundRobin)
	if err := store.Add(&auth.TokenData{
		ID: "A", Provider: "claude", AccessToken: "token-A",
		ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	wantBody := `{"type":"error","error":{"type":"invalid_request_error","message":"You're out of extra usage. Ask your workspace admin to add more so you can keep going."},"request_id":"req_test"}`
	executor := NewClaudeOAuthExecutor(auth.NewClaudeOAuth(store), nil)
	executor.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(wantBody)),
			Request:    req,
		}, nil
	})}

	resp, err := executor.doWithFailover(context.Background(), "claude-opus-5", func(token string) (*http.Request, error) {
		return http.NewRequest(http.MethodPost, "https://example.test/messages", nil)
	})
	if err != nil {
		t.Fatalf("doWithFailover: %v", err)
	}
	defer resp.Body.Close()
	gotBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read final response: %v", err)
	}
	if string(gotBody) != wantBody {
		t.Fatalf("body=%q want %q", gotBody, wantBody)
	}
}
