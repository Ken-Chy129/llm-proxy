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
	if got := store.Get("claude", ""); got == nil || got.ID != "B" {
		t.Fatalf("expected B (soonest weekly reset), got %v", got)
	}

	// B's weekly exhausted → B excluded from the quota tier → prefer A.
	setQuota("B", now.Add(3*time.Hour), now.Add(1*24*time.Hour), false, true)
	if got := store.Get("claude", ""); got == nil || got.ID != "A" {
		t.Fatalf("expected A after B weekly exhausted, got %v", got)
	}

	// A's session also exhausted → both blocked in the quota tier → fall through
	// to round-robin, which still returns some active, non-rate-limited account.
	setQuota("A", now.Add(3*time.Hour), now.Add(6*24*time.Hour), true, false)
	if got := store.Get("claude", ""); got == nil {
		t.Fatal("expected round-robin fallback to return an account, got nil")
	}
}

// A per-model reactive cooldown (e.g. hitting the Fable weekly cap) must skip
// the account only for that model — the account stays selectable for others —
// and must not surface as an account-wide "limited" via RateLimitInfo. An
// account-wide cooldown (model "") blocks every model and does surface.
func TestPerModelRateLimit(t *testing.T) {
	dir := t.TempDir()
	InitQuotaCache(dir)
	store := NewTokenStore(dir, StrategyRoundRobin)

	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	for _, id := range []string{"A", "B"} {
		store.Add(&TokenData{ID: id, Provider: "claude", AccessToken: "t-" + id, ExpiresAt: future})
	}

	// Bench A only for claude-fable-5. (B stays free, so selection has a choice
	// and never falls through to the "return anything" last resort.)
	store.MarkRateLimited("claude", "A", "claude-fable-5", time.Now().Add(5*time.Minute), false)

	fable, sonnet := map[string]bool{}, map[string]bool{}
	for i := 0; i < 10; i++ {
		if g := store.Get("claude", "claude-fable-5"); g != nil {
			fable[g.ID] = true
		}
		if g := store.Get("claude", "claude-sonnet-5"); g != nil {
			sonnet[g.ID] = true
		}
	}
	if fable["A"] {
		t.Error("A must be skipped for the benched model claude-fable-5")
	}
	if !fable["B"] {
		t.Error("B must serve claude-fable-5")
	}
	if !sonnet["A"] {
		t.Error("A must stay selectable for other models")
	}
	// Per-model bench must not drive the account-level badge.
	if _, _, active := store.RateLimitInfo("claude", "A"); active {
		t.Error("per-model cooldown must not show as an account-wide limit")
	}

	// An account-wide bench blocks every model on A and surfaces via RateLimitInfo.
	store.MarkRateLimited("claude", "A", "", time.Now().Add(5*time.Minute), false)
	sonnet = map[string]bool{}
	for i := 0; i < 10; i++ {
		if g := store.Get("claude", "claude-sonnet-5"); g != nil {
			sonnet[g.ID] = true
		}
	}
	if sonnet["A"] {
		t.Error("account-wide bench must block all models on A")
	}
	if _, _, active := store.RateLimitInfo("claude", "A"); !active {
		t.Error("account-wide cooldown must surface via RateLimitInfo")
	}
}

func TestRateWindowExhausted(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		w    *RateWindow
		want bool
	}{
		{"nil", nil, false},
		{"not limited", &RateWindow{LimitReached: false, ResetUnix: now.Add(time.Hour).Unix()}, false},
		{"limited, reset future", &RateWindow{LimitReached: true, ResetUnix: now.Add(time.Hour).Unix()}, true},
		{"limited, reset passed (stale)", &RateWindow{LimitReached: true, ResetUnix: now.Add(-time.Hour).Unix()}, false},
		{"limited, reset unknown", &RateWindow{LimitReached: true, ResetUnix: 0}, true},
	}
	for _, c := range cases {
		if got := c.w.Exhausted(now); got != c.want {
			t.Errorf("%s: Exhausted=%v want %v", c.name, got, c.want)
		}
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
		if got := store.Get("claude", ""); got != nil {
			seen[got.ID] = true
		}
	}
	if !seen["A"] || !seen["B"] {
		t.Fatalf("round_robin should touch both accounts, saw %v", seen)
	}
}
