package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func withClaudeTokenEndpoint(t *testing.T, url string) {
	t.Helper()
	orig := ClaudeTokenURL
	ClaudeTokenURL = url
	t.Cleanup(func() { ClaudeTokenURL = orig })
}

// Claude hands out refresh tokens that expire. When one does, every request
// routed to that account fails, so it has to be marked rather than left looking
// healthy — that was the state the dashboard misreported: a green dot on an
// account whose grant died weeks earlier.
func TestClaudeRefreshRejectionMarksRevoked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant","error_description":"Refresh token expired"}`)
	}))
	defer srv.Close()
	withClaudeTokenEndpoint(t, srv.URL)

	dir := t.TempDir()
	InitQuotaCache(dir)
	store := NewTokenStore(dir, StrategyRoundRobin)
	oauth := NewClaudeOAuth(store)
	oauth.SetHTTPClient(http.DefaultClient)

	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	if err := store.Add(&TokenData{
		ID: "a@b.com", Provider: "claude", AccessToken: "old",
		RefreshToken: "dead", ExpiresAt: past,
	}); err != nil {
		t.Fatalf("add: %v", err)
	}

	if _, err := oauth.GetToken(context.Background()); err == nil {
		t.Fatal("expected the refresh to fail")
	}
	if !store.IsRevoked("claude", "a@b.com") {
		t.Fatal("an expired refresh token must mark the account revoked")
	}
	if store.ActiveCount("claude") != 0 {
		t.Fatalf("ActiveCount = %d, want 0", store.ActiveCount("claude"))
	}
}

// A successful refresh must keep the account under its original id. The upstream
// response does not always carry the email, and rebuilding the id from it would
// mint a second account while the first stayed marked revoked.
func TestClaudeRefreshKeepsAccountIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"fresh","refresh_token":"r2","expires_in":3600}`)
	}))
	defer srv.Close()
	withClaudeTokenEndpoint(t, srv.URL)

	dir := t.TempDir()
	InitQuotaCache(dir)
	store := NewTokenStore(dir, StrategyRoundRobin)
	oauth := NewClaudeOAuth(store)
	oauth.SetHTTPClient(http.DefaultClient)

	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	if err := store.Add(&TokenData{
		ID: "a@b.com", Provider: "claude", Email: "a@b.com", AccessToken: "old",
		RefreshToken: "live", ExpiresAt: past,
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := store.MarkRevoked("claude", "a@b.com", "stale mark"); err != nil {
		t.Fatalf("mark: %v", err)
	}

	if err := oauth.ForceRefresh(context.Background(), "a@b.com"); err != nil {
		t.Fatalf("force refresh: %v", err)
	}

	accounts := store.AllForProvider("claude")
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1 (the refresh must update, not duplicate)", len(accounts))
	}
	if accounts[0].ID != "a@b.com" || accounts[0].AccessToken != "fresh" {
		t.Fatalf("account = %+v, want the same id holding the new token", accounts[0])
	}
	if store.IsRevoked("claude", "a@b.com") {
		t.Fatal("a working refresh must clear the revoked mark")
	}
}

// ForceRefresh exists for the 401 case, where the stored token still looks
// valid locally: it must not take the "already refreshed" shortcut.
func TestClaudeForceRefreshIgnoresLocalExpiry(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"fresh","refresh_token":"r2","expires_in":3600}`)
	}))
	defer srv.Close()
	withClaudeTokenEndpoint(t, srv.URL)

	dir := t.TempDir()
	InitQuotaCache(dir)
	store := NewTokenStore(dir, StrategyRoundRobin)
	oauth := NewClaudeOAuth(store)
	oauth.SetHTTPClient(http.DefaultClient)

	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	if err := store.Add(&TokenData{
		ID: "a@b.com", Provider: "claude", Email: "a@b.com", AccessToken: "rejected",
		RefreshToken: "live", ExpiresAt: future,
	}); err != nil {
		t.Fatalf("add: %v", err)
	}

	if err := oauth.ForceRefresh(context.Background(), "a@b.com"); err != nil {
		t.Fatalf("force refresh: %v", err)
	}
	if calls != 1 {
		t.Fatalf("token endpoint calls = %d, want 1 despite the token looking valid", calls)
	}
	if got := store.GetByID("claude", "a@b.com"); got == nil || got.AccessToken != "fresh" {
		t.Fatalf("account = %+v, want the refreshed token", got)
	}
}
