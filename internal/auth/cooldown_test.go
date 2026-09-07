package auth

import (
	"testing"
	"time"
)

// A cooldown outlives the 429 that created it, so an account topped up before
// the upstream's advertised reset time must be reclaimable without a restart.
func TestClearRateLimitRestoresSelection(t *testing.T) {
	dir := t.TempDir()
	InitQuotaCache(dir)
	store := NewTokenStore(dir, StrategyRoundRobin)

	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	store.Add(&TokenData{ID: "A", Provider: "codex", AccessToken: "t-A", ExpiresAt: future})
	// B keeps selection honest: with only one account, Get falls back to
	// "return anything rather than nothing" and would hand back A regardless.
	store.Add(&TokenData{ID: "B", Provider: "codex", AccessToken: "t-B", ExpiresAt: future})

	// A week-long cooldown, the shape that stranded gpt-6-astra on anygen.
	store.MarkRateLimited("codex", "A", "", time.Now().Add(7*24*time.Hour), false)
	for i := 0; i < 10; i++ {
		if got := store.Get("codex", "gpt-6-astra"); got != nil && got.ID == "A" {
			t.Fatal("a cooling-down account must not be selected while B is free")
		}
	}
	if _, _, active := store.RateLimitInfo("codex", "A"); !active {
		t.Fatal("cooldown must be active before it is cleared")
	}

	if !store.ClearRateLimit("codex", "A") {
		t.Fatal("ClearRateLimit() = false, want it to report the cooldown removed")
	}

	if _, _, active := store.RateLimitInfo("codex", "A"); active {
		t.Error("cooldown must be gone after ClearRateLimit")
	}
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		if got := store.Get("codex", "gpt-6-astra"); got != nil {
			seen[got.ID] = true
		}
	}
	if !seen["A"] {
		t.Error("A must be selectable again once its cooldown is cleared")
	}
}

// Per-model benches live under different keys than the account-wide one; a
// clear must take all of them, or the account stays half-sidelined.
func TestClearRateLimitDropsPerModelCooldowns(t *testing.T) {
	dir := t.TempDir()
	InitQuotaCache(dir)
	store := NewTokenStore(dir, StrategyRoundRobin)

	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	store.Add(&TokenData{ID: "A", Provider: "claude", AccessToken: "t-A", ExpiresAt: future})
	store.Add(&TokenData{ID: "B", Provider: "claude", AccessToken: "t-B", ExpiresAt: future})

	store.MarkRateLimited("claude", "A", "", time.Now().Add(time.Hour), false)
	store.MarkRateLimited("claude", "A", "claude-fable-5", time.Now().Add(time.Hour), false)

	if !store.ClearRateLimit("claude", "A") {
		t.Fatal("ClearRateLimit() = false, want cooldowns removed")
	}
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		if got := store.Get("claude", "claude-fable-5"); got != nil {
			seen[got.ID] = true
		}
	}
	if !seen["A"] {
		t.Error("A must serve the previously benched model once both cooldowns are cleared")
	}
}

func TestClearRateLimitLeavesOtherAccountsAlone(t *testing.T) {
	dir := t.TempDir()
	InitQuotaCache(dir)
	store := NewTokenStore(dir, StrategyRoundRobin)

	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	for _, id := range []string{"A", "B"} {
		store.Add(&TokenData{ID: id, Provider: "codex", AccessToken: "t-" + id, ExpiresAt: future})
	}
	store.MarkRateLimited("codex", "A", "", time.Now().Add(time.Hour), false)
	store.MarkRateLimited("codex", "B", "", time.Now().Add(time.Hour), false)

	store.ClearRateLimit("codex", "A")

	if _, _, active := store.RateLimitInfo("codex", "B"); !active {
		t.Error("B's cooldown must survive a clear aimed at A")
	}
	if store.ClearRateLimit("codex", "A") {
		t.Error("clearing an account with no cooldown must report false")
	}
}

// HasHeadroom decides whether a refresh retires a cooldown, so a snapshot that
// proves nothing must never read as "recovered".
func TestHasHeadroom(t *testing.T) {
	now := time.Now()
	soon := now.Add(time.Hour).Unix()
	past := now.Add(-time.Hour).Unix()

	tests := []struct {
		name string
		info *QuotaInfo
		want bool
	}{
		{
			name: "fresh data with room",
			info: &QuotaInfo{HasRealData: true, Primary: &RateWindow{RemainingPercent: 100}},
			want: true,
		},
		{
			name: "exhausted primary",
			info: &QuotaInfo{HasRealData: true, Primary: &RateWindow{LimitReached: true, ResetUnix: soon}},
			want: false,
		},
		{
			name: "exhausted secondary",
			info: &QuotaInfo{
				HasRealData: true,
				Primary:     &RateWindow{RemainingPercent: 100},
				Secondary:   &RateWindow{LimitReached: true, ResetUnix: soon},
			},
			want: false,
		},
		{
			name: "reset time already passed counts as room",
			info: &QuotaInfo{HasRealData: true, Primary: &RateWindow{LimitReached: true, ResetUnix: past}},
			want: true,
		},
		{
			name: "exhausted additional window",
			info: &QuotaInfo{
				HasRealData: true,
				Primary:     &RateWindow{RemainingPercent: 100},
				Additional:  []AdditionalRL{{Name: "opus", Primary: &RateWindow{LimitReached: true, ResetUnix: soon}}},
			},
			want: false,
		},
		{
			name: "placeholder from a failed fetch proves nothing",
			info: &QuotaInfo{HasRealData: false, Primary: &RateWindow{RemainingPercent: 100}},
			want: false,
		},
		{
			name: "nil",
			info: nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.info.HasHeadroom(now); got != tt.want {
				t.Fatalf("HasHeadroom() = %v, want %v", got, tt.want)
			}
		})
	}
}
