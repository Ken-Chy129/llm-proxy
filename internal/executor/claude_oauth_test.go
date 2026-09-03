package executor

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Ken-Chy129/llm-proxy/internal/auth"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func claudeVersionValue(t *testing.T, version string) int {
	t.Helper()
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		t.Fatalf("invalid Claude Code version %q", version)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		t.Fatalf("invalid Claude Code major version %q: %v", version, err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("invalid Claude Code minor version %q: %v", version, err)
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		t.Fatalf("invalid Claude Code patch version %q: %v", version, err)
	}
	return major*1_000_000 + minor*1_000 + patch
}

func TestClaudeCodeIdentityVersionMeetsAnthropicModelFloor(t *testing.T) {
	const minimumVersion = "2.1.251"

	if got := claudeVersionValue(t, claudeCodeVersion); got < claudeVersionValue(t, minimumVersion) {
		t.Fatalf("Claude Code identity version=%q is older than required minimum %q", claudeCodeVersion, minimumVersion)
	}
	if got, want := claudeOAuthUserAgent, "claude-cli/"+claudeCodeVersion+" (external, sdk-cli)"; got != want {
		t.Fatalf("Claude OAuth User-Agent=%q want %q", got, want)
	}
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

func TestClaudeExtraUsage400DoesNotCooldownOrFailOver(t *testing.T) {
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
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"type":"error","error":{"type":"invalid_request_error","message":"You're out of extra usage. Ask your workspace admin to add more so you can keep going."}}`,
			)),
			Request: req,
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
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want %d", resp.StatusCode, http.StatusBadRequest)
	}
	if len(tokens) != 1 {
		t.Fatalf("extra-usage 400 must not fail over accounts, attempts=%v", tokens)
	}

	account, failedOver := accountResult()
	if account == "" || len(failedOver) != 0 {
		t.Fatalf("account=%q failedOver=%v", account, failedOver)
	}
	if _, _, active := store.RateLimitInfo("claude", account); active {
		t.Fatalf("request-specific extra-usage 400 must not cooldown account %q", account)
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

func TestAnthropicPassthroughHeadersPreserveOAuthIdentity(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.test/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	clientHeaders := make(http.Header)
	clientHeaders.Set("anthropic-version", "2023-06-01")
	clientHeaders.Set("anthropic-beta", "interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14")
	clientHeaders.Set("Authorization", "Bearer untrusted-client-token")
	clientHeaders.Set("x-api-key", "untrusted-client-key")

	applyAnthropicPassthroughHeaders(req, "oauth-token", clientHeaders)

	betas := strings.Split(req.Header.Get("anthropic-beta"), ",")
	for _, want := range []string{
		"interleaved-thinking-2025-05-14",
		"fine-grained-tool-streaming-2025-05-14",
		"claude-code-20250219",
		"oauth-2025-04-20",
	} {
		count := 0
		for _, got := range betas {
			if got == want {
				count++
			}
		}
		if count != 1 {
			t.Errorf("beta %q count=%d in %q", want, count, req.Header.Get("anthropic-beta"))
		}
	}
	if got, want := req.Header.Get("User-Agent"), "claude-cli/"+claudeCodeVersion+" (external, sdk-cli)"; got != want {
		t.Errorf("User-Agent=%q want %q", got, want)
	}
	if got := req.Header.Get("x-app"); got != "cli" {
		t.Errorf("x-app=%q want cli", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer oauth-token" {
		t.Errorf("Authorization=%q want upstream OAuth token", got)
	}
	if got := req.Header.Get("x-api-key"); got != "" {
		t.Errorf("untrusted client x-api-key leaked upstream: %q", got)
	}
}

func TestPassthroughBetaDoesNotLeakIntoDashboardRequests(t *testing.T) {
	dir := t.TempDir()
	store := auth.NewTokenStore(dir, auth.StrategyRoundRobin)
	if err := store.Add(&auth.TokenData{
		ID: "A", Provider: "claude", AccessToken: "token-A",
		ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	executor := NewClaudeOAuthExecutor(auth.NewClaudeOAuth(store), nil)
	executor.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	})}

	passthroughHeaders := make(http.Header)
	passthroughHeaders.Set("anthropic-beta", "hermes-only-beta")
	stream, status, err := executor.OpenAnthropicStream(
		context.Background(),
		[]byte(`{"model":"claude-opus-5","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`),
		passthroughHeaders,
	)
	if err != nil {
		t.Fatalf("OpenAnthropicStream: %v", err)
	}
	stream.Close()
	if status != http.StatusOK {
		t.Fatalf("status=%d want %d", status, http.StatusOK)
	}

	dashboardReq, _ := http.NewRequest(http.MethodPost, "https://example.test/messages", nil)
	executor.applyHeaders(dashboardReq, "oauth-token")
	if got := dashboardReq.Header.Get("anthropic-beta"); strings.Contains(got, "hermes-only-beta") {
		t.Fatalf("passthrough beta leaked into dashboard request: %q", got)
	}
}
