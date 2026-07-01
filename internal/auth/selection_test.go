package auth

import (
	"testing"
	"time"
)

func TestGetWeeklyExpirySelection(t *testing.T) {
	dir := t.TempDir()
	InitQuotaCache(dir)
	store := NewTokenStore(dir, StrategyWeeklyExpiry)

	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	for _, id := range []string{"A", "B"} {
		if err := store.Add(&TokenData{ID: id, Provider: "claude", AccessToken: "t-" + id, ExpiresAt: future}); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}

	now := time.Now()
	setQuota := func(id string, sessionReset, weeklyReset time.Time, sessLimited, weekLimited bool) {
		QuotaCache.Set("claude:"+id, &QuotaInfo{
			AccountID:   id,
			HasRealData: true,
			Primary:     &RateWindow{Label: "session", ResetUnix: sessionReset.Unix(), LimitReached: sessLimited},
			Secondary:   &RateWindow{Label: "weekly", ResetUnix: weeklyReset.Unix(), LimitReached: weekLimited},
		})
	}

	// A weekly resets in 6 days, B in 1 day → prefer B (burn soonest-expiring weekly).
	setQuota("A", now.Add(3*time.Hour), now.Add(6*24*time.Hour), false, false)
	setQuota("B", now.Add(3*time.Hour), now.Add(1*24*time.Hour), false, false)
	if got := store.Get("claude"); got == nil || got.ID != "B" {
		t.Fatalf("expected B (soonest weekly reset), got %v", got)
	}

	// B's weekly exhausted → B excluded from the quota tier → prefer A.
	setQuota("B", now.Add(3*time.Hour), now.Add(1*24*time.Hour), false, true)
	if got := store.Get("claude"); got == nil || got.ID != "A" {
		t.Fatalf("expected A after B weekly exhausted, got %v", got)
	}

	// A's session also exhausted → both blocked in the quota tier → fall through
	// to round-robin, which still returns some active, non-rate-limited account.
	setQuota("A", now.Add(3*time.Hour), now.Add(6*24*time.Hour), true, false)
	if got := store.Get("claude"); got == nil {
		t.Fatal("expected round-robin fallback to return an account, got nil")
	}
}

func TestGetRoundRobinIgnoresQuota(t *testing.T) {
	dir := t.TempDir()
	InitQuotaCache(dir)
	store := NewTokenStore(dir, StrategyRoundRobin)

	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	for _, id := range []string{"A", "B"} {
		store.Add(&TokenData{ID: id, Provider: "claude", AccessToken: "t-" + id, ExpiresAt: future})
	}
	// Even with lopsided quota, round_robin must rotate across both accounts.
	QuotaCache.Set("claude:A", &QuotaInfo{AccountID: "A", HasRealData: true,
		Secondary: &RateWindow{ResetUnix: time.Now().Add(24 * time.Hour).Unix()}})

	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		if got := store.Get("claude"); got != nil {
			seen[got.ID] = true
		}
	}
	if !seen["A"] || !seen["B"] {
		t.Fatalf("round_robin should touch both accounts, saw %v", seen)
	}
}
