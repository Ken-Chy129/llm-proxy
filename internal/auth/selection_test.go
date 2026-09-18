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

	// A's session also exhausted → every account is out of quota. Selection
	// must say so (nil) rather than hand one back to collect a certain 429;
	// the chain then fails over to the next provider without the detour.
	setQuota("A", now.Add(3*time.Hour), now.Add(6*24*time.Hour), true, false)
	if got := store.Get("claude", ""); got != nil {
		t.Fatalf("expected nil when every account is exhausted, got %v", got.ID)
	}

	// The moment a window's reset passes, the stale snapshot stops blocking.
	setQuota("A", now.Add(-time.Minute), now.Add(6*24*time.Hour), true, false)
	if got := store.Get("claude", ""); got == nil || got.ID != "A" {
		t.Fatalf("expected A once its session reset passed, got %v", got)
	}
}

// Every account cooling down after a 429 used to be handed back anyway by the
// "always try something" fallback, so a request against a fully limited pool
// walked all N accounts and collected N guaranteed 429s before the chain moved
// on. Selection must return nil instead, and recover when a cooldown lapses.
func TestGetReturnsNilWhenEveryAccountIsCoolingDown(t *testing.T) {
	dir := t.TempDir()
	InitQuotaCache(dir)
	store := NewTokenStore(dir, StrategyWeeklyExpiry)
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	for _, id := range []string{"A", "B"} {
		store.Add(&TokenData{ID: id, Provider: "codex", AccessToken: "t-" + id, ExpiresAt: future})
	}
	store.MarkRateLimited("codex", "A", "", time.Now().Add(20*time.Minute), false)
	store.MarkRateLimited("codex", "B", "", time.Now().Add(time.Hour), false)
	if got := store.Get("codex", ""); got != nil {
		t.Fatalf("expected nil while both accounts cool down, got %v", got.ID)
	}
	// An expired-but-not-limited token is still worth returning: the caller
	// refreshes it.
	store.ClearRateLimit("codex", "A")
	if got := store.Get("codex", ""); got == nil || got.ID != "A" {
		t.Fatalf("expected A after its cooldown cleared, got %v", got)
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

// A quota snapshot whose model-scoped weekly window (Fable) is spent must keep
// the account out of Fable requests, under both strategies, while it still
// serves other models. Reactive 429 cooldowns alone cannot do this: they are
// clamped to minutes, so without the quota signal each Fable request would
// re-try the dead account and burn an attempt on a guaranteed 429.
func TestModelScopedQuotaSkipsAccountForThatModel(t *testing.T) {
	for _, strategy := range []string{StrategyWeeklyExpiry, StrategyRoundRobin} {
		t.Run(strategy, func(t *testing.T) {
			dir := t.TempDir()
			InitQuotaCache(dir)
			store := NewTokenStore(dir, strategy)
			future := time.Now().Add(time.Hour).Format(time.RFC3339)
			for _, id := range []string{"A", "B"} {
				store.Add(&TokenData{ID: id, Provider: "claude", AccessToken: "t-" + id, ExpiresAt: future})
			}
			now := time.Now()
			mk := func(id string, fableSpent bool) *QuotaInfo {
				return &QuotaInfo{
					AccountID: id, HasRealData: true,
					Primary:   &RateWindow{Label: labelClaudeSession, ResetUnix: now.Add(3 * time.Hour).Unix()},
					Secondary: &RateWindow{Label: labelClaudeWeeklyAll, ResetUnix: now.Add(24 * time.Hour).Unix()},
					Additional: []AdditionalRL{{Name: "Fable weekly", Primary: &RateWindow{
						Label: "Fable weekly", Model: "fable", LimitReached: fableSpent,
						ResetUnix: now.Add(72 * time.Hour).Unix(),
					}}},
				}
			}
			// A resets its weekly sooner, so weekly_expiry would prefer it if
			// Fable were not spent there.
			a := mk("A", true)
			a.Secondary.ResetUnix = now.Add(12 * time.Hour).Unix()
			QuotaCache.Set("claude:A", a)
			QuotaCache.Set("claude:B", mk("B", false))

			fable, opus := map[string]bool{}, map[string]bool{}
			for i := 0; i < 10; i++ {
				if g := store.Get("claude", "claude-fable-5-1"); g != nil {
					fable[g.ID] = true
				}
				if g := store.Get("claude", "claude-opus-5"); g != nil {
					opus[g.ID] = true
				}
			}
			if fable["A"] {
				t.Error("A must be skipped for Fable while its Fable weekly is spent")
			}
			if !fable["B"] {
				t.Error("B must serve Fable")
			}
			if !opus["A"] {
				t.Error("A must stay selectable for other models")
			}
			if _, _, active := store.RateLimitInfo("claude", "A"); active {
				t.Error("a model-scoped quota cap must not show as an account-wide limit")
			}
		})
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
