package auth

import (
	"testing"
	"time"
)

// A revoked account is one the upstream itself rejected: selection must skip it
// entirely, including the "try anything rather than nothing" fallback tiers,
// since only a re-login can make it work again.
func TestRevokedAccountIsNeverSelected(t *testing.T) {
	dir := t.TempDir()
	InitQuotaCache(dir)
	store := NewTokenStore(dir, StrategyRoundRobin)

	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	for _, id := range []string{"A", "B"} {
		if err := store.Add(&TokenData{ID: id, Provider: "codex", AccessToken: "t-" + id, ExpiresAt: future}); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}

	if err := store.MarkRevoked("codex", "A", `{"error":{"message":"Encountered invalidated oauth token for user","code":"token_revoked"}}`); err != nil {
		t.Fatalf("mark revoked: %v", err)
	}

	for i := 0; i < 6; i++ {
		got := store.Get("codex", "")
		if got == nil {
			t.Fatal("expected B to remain selectable, got nil")
		}
		if got.ID == "A" {
			t.Fatalf("selected revoked account A on iteration %d", i)
		}
	}

	// With every account revoked there is nothing to serve: returning a token
	// known to be dead would spend the request on a guaranteed 401 instead of
	// letting the chain fail over to another provider.
	if err := store.MarkRevoked("codex", "B", "revoked"); err != nil {
		t.Fatalf("mark revoked: %v", err)
	}
	if got := store.Get("codex", ""); got != nil {
		t.Fatalf("expected nil when all accounts are revoked, got %s", got.ID)
	}
	if store.ActiveCount("codex") != 0 {
		t.Fatalf("ActiveCount = %d, want 0 when every account is revoked", store.ActiveCount("codex"))
	}
}

// The revoked mark must outlive a restart, or the dashboard would show a dead
// account as healthy again until the next request fails.
func TestRevokedStatePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	InitQuotaCache(dir)
	store := NewTokenStore(dir, StrategyRoundRobin)

	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	if err := store.Add(&TokenData{ID: "A", Provider: "codex", AccessToken: "t-A", ExpiresAt: future}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := store.MarkRevoked("codex", "A", "token_revoked"); err != nil {
		t.Fatalf("mark revoked: %v", err)
	}

	reopened := NewTokenStore(dir, StrategyRoundRobin)
	at, reason, revoked := reopened.RevokedInfo("codex", "A")
	if !revoked {
		t.Fatal("expected the revoked mark to survive a restart")
	}
	if reason != "token_revoked" {
		t.Fatalf("reason = %q, want the upstream message preserved", reason)
	}
	if at.IsZero() {
		t.Fatal("expected a revocation timestamp")
	}

	// A successful refresh or re-login clears it.
	if err := reopened.ClearRevoked("codex", "A"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if reopened.IsRevoked("codex", "A") {
		t.Fatal("expected the account to be usable after clearing")
	}
	if NewTokenStore(dir, StrategyRoundRobin).IsRevoked("codex", "A") {
		t.Fatal("expected the cleared state to persist too")
	}
}
