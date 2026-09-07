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
	"time"

	"github.com/Ken-Chy129/llm-proxy/internal/auth"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

func testChatRequest() *types.ChatCompletionRequest {
	content, _ := json.Marshal("hi")
	return &types.ChatCompletionRequest{
		Model:    "gpt-5.5",
		Messages: []types.ChatMessage{{Role: "user", Content: content}},
	}
}

const revokedBody = `{"error":{"message":"Encountered invalidated oauth token for user, failing request","code":"token_revoked"}}`

// withCodexUpstream points the executor and the token endpoint at stubs.
func withCodexUpstream(t *testing.T, upstream, tokenEndpoint string) {
	t.Helper()
	origBase, origToken := codexBaseURL, auth.CodexTokenURL
	codexBaseURL = upstream
	auth.CodexTokenURL = tokenEndpoint
	t.Cleanup(func() {
		codexBaseURL = origBase
		auth.CodexTokenURL = origToken
	})
}

func newCodexTestExecutor(t *testing.T, ids ...string) (*CodexExecutor, *auth.TokenStore) {
	t.Helper()
	dir := t.TempDir()
	auth.InitQuotaCache(dir)
	store := auth.NewTokenStore(dir, auth.StrategyRoundRobin)
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	for _, id := range ids {
		if err := store.Add(&auth.TokenData{
			ID: id, Provider: "codex", AccessToken: "access-" + id,
			RefreshToken: "refresh-" + id, ExpiresAt: future,
		}); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}
	oauth := auth.NewCodexOAuth(store)
	oauth.SetHTTPClient(http.DefaultClient)
	exec := NewCodexExecutor(oauth, []string{"gpt-5.5"})
	exec.httpClient = http.DefaultClient
	return exec, store
}

// A 401 usually just means the access token was rotated away by another client,
// so the first response is to refresh and retry the same account rather than
// declare it dead.
func TestCodexRefreshesOnceOn401(t *testing.T) {
	var mu sync.Mutex
	var seenTokens []string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		seenTokens = append(seenTokens, token)
		mu.Unlock()
		if token == "refreshed-token" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, revokedBody)
	}))
	defer upstream.Close()

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"refreshed-token","refresh_token":"r2","expires_in":3600}`)
	}))
	defer tokenSrv.Close()

	withCodexUpstream(t, upstream.URL, tokenSrv.URL)
	exec, store := newCodexTestExecutor(t, "A")

	var out strings.Builder
	if _, err := exec.ExecuteStream(context.Background(), testChatRequest(), &out); err != nil {
		t.Fatalf("expected the retry after refresh to succeed, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seenTokens) != 2 {
		t.Fatalf("upstream calls = %d (%v), want 2: the original and the retry", len(seenTokens), seenTokens)
	}
	if seenTokens[1] != "refreshed-token" {
		t.Fatalf("retry used %q, want the refreshed token", seenTokens[1])
	}
	if store.IsRevoked("codex", "A") {
		t.Fatal("account must not be marked revoked when the refresh fixed it")
	}
}

// When the refresh is rejected too, the grant itself is gone: mark the account
// so the dashboard can ask for a re-login, and fail over to the next account.
func TestCodexMarksRevokedAndFailsOverWhenRefreshFails(t *testing.T) {
	var mu sync.Mutex
	// Reject whichever account is tried first and accept the next one, so the
	// test asserts the failover behaviour without depending on the rotation
	// order the store happens to pick.
	var rejected string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		if rejected == "" {
			rejected = token
		}
		bad := token == rejected
		mu.Unlock()
		if bad {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, revokedBody)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant"}`)
	}))
	defer tokenSrv.Close()

	withCodexUpstream(t, upstream.URL, tokenSrv.URL)
	exec, store := newCodexTestExecutor(t, "A", "B")

	var out strings.Builder
	if _, err := exec.ExecuteStream(context.Background(), testChatRequest(), &out); err != nil {
		t.Fatalf("expected failover to the healthy account, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	revokedID := strings.TrimPrefix(rejected, "access-")
	healthyID := "A"
	if revokedID == "A" {
		healthyID = "B"
	}
	if !store.IsRevoked("codex", revokedID) {
		t.Fatalf("account %s got a 401 and a failed refresh, so it should be marked revoked", revokedID)
	}
	if store.IsRevoked("codex", healthyID) {
		t.Fatalf("account %s served the request and must stay usable", healthyID)
	}
}

// With every account revoked the executor reports the upstream 401 rather than
// a generic failure, so the chain can fail over to another provider and the
// logs name the real cause.
func TestCodexSurfaces401WhenAllAccountsRevoked(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, revokedBody)
	}))
	defer upstream.Close()

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant"}`)
	}))
	defer tokenSrv.Close()

	withCodexUpstream(t, upstream.URL, tokenSrv.URL)
	exec, store := newCodexTestExecutor(t, "A")

	var out strings.Builder
	_, err := exec.ExecuteStream(context.Background(), testChatRequest(), &out)
	if err == nil {
		t.Fatal("expected an error when the only account is revoked")
	}
	if got := StatusFromError(err); got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 so the chain treats it as a credential failure", got)
	}
	if !store.IsRevoked("codex", "A") {
		t.Fatal("account should be marked revoked")
	}
}
