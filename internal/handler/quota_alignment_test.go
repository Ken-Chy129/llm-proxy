package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Ken-Chy129/llm-proxy/internal/auth"
	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
	"github.com/Ken-Chy129/llm-proxy/internal/stats"
	"github.com/gin-gonic/gin"
)

// quotaStatusFixture stands up a Status handler over throwaway state with two
// Claude accounts: one healthy, one the upstream has revoked.
func quotaStatusFixture(t *testing.T) (*AdminHandler, *auth.TokenStore) {
	t.Helper()
	dir := t.TempDir()
	auth.InitQuotaCache(dir)
	store := auth.NewTokenStore(dir, "")
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	for _, id := range []string{"live@example.com", "dead@example.com"} {
		if err := store.Add(&auth.TokenData{
			ID: id, Provider: "claude", Email: id,
			AccessToken: "t", RefreshToken: "r", ExpiresAt: future,
		}); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
		auth.QuotaCache.Set("claude:"+id, &auth.QuotaInfo{
			AccountID:   id,
			Email:       id,
			PlanType:    "Max 5x",
			HasRealData: true,
			Primary:     &auth.RateWindow{Label: "Current session (5h)", RemainingPercent: 100},
			Secondary:   &auth.RateWindow{Label: "Weekly (all models)", RemainingPercent: 100},
		})
	}

	db, err := stats.Open(dir)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := &config.Config{ClaudeOAuth: config.ClaudeOAuthConfig{Enabled: true}}
	h := &AdminHandler{cfg: cfg, router: router.New(), tokenStore: store, statsDB: db}
	return h, store
}

// statusQuotas runs Status and returns the quota cards for the claude backend.
func statusQuotas(t *testing.T, h *AdminHandler) []map[string]any {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	h.Status(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Backends []struct {
			Name   string           `json:"name"`
			Quotas []map[string]any `json:"quotas"`
		} `json:"backends"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, b := range body.Backends {
		if b.Name == "claude_oauth" {
			return b.Quotas
		}
	}
	t.Fatal("claude_oauth backend missing from status")
	return nil
}

func quotaFor(t *testing.T, cards []map[string]any, account string) map[string]any {
	t.Helper()
	for _, c := range cards {
		if c["account_id"] == account {
			return c
		}
	}
	t.Fatalf("no quota card for %s", account)
	return nil
}

// The Providers tab knows an account was revoked; the Quota tab used to keep
// drawing its last snapshot as full green bars. Both tabs describe the same
// account, so the quota card has to carry that account's reachability.
func TestQuotaCardsCarryAccountServingState(t *testing.T) {
	h, store := quotaStatusFixture(t)
	if err := store.MarkRevoked("claude", "dead@example.com", "401 invalid_grant"); err != nil {
		t.Fatalf("MarkRevoked: %v", err)
	}

	cards := statusQuotas(t, h)
	if len(cards) != 2 {
		t.Fatalf("got %d quota cards, want 2", len(cards))
	}

	live := quotaFor(t, cards, "live@example.com")
	if live["serving"] != true {
		t.Errorf("healthy account serving = %v, want true", live["serving"])
	}
	if live["account_state"] != "active" {
		t.Errorf("healthy account_state = %v, want active", live["account_state"])
	}

	if live["tier"] != "serving" {
		t.Errorf("healthy tier = %v, want serving", live["tier"])
	}

	dead := quotaFor(t, cards, "dead@example.com")
	if dead["serving"] != false {
		t.Errorf("revoked account still advertises quota: serving = %v", dead["serving"])
	}
	if dead["account_state"] != "revoked" {
		t.Errorf("revoked account_state = %v, want revoked", dead["account_state"])
	}
	// A revoked account cannot recover on its own, so it must not be filed with
	// the rate-limited ones that clear themselves.
	if dead["tier"] != "blocked" {
		t.Errorf("revoked tier = %v, want blocked", dead["tier"])
	}
	if detail, _ := dead["state_detail"].(string); detail == "" {
		t.Error("revoked quota card gives no reason the account cannot serve")
	}
}

// Pausing an account on the Providers tab is the same claim as "this cannot
// serve", so the quota card must not keep presenting it as capacity.
func TestPausedAccountQuotaIsNotPresentedAsServing(t *testing.T) {
	h, store := quotaStatusFixture(t)
	if err := store.DisableAccount("claude", "dead@example.com"); err != nil {
		t.Fatalf("DisableAccount: %v", err)
	}

	dead := quotaFor(t, statusQuotas(t, h), "dead@example.com")
	if dead["serving"] != false {
		t.Errorf("paused account serving = %v, want false", dead["serving"])
	}
	if dead["account_state"] != "disabled" {
		t.Errorf("paused account_state = %v, want disabled", dead["account_state"])
	}
	if dead["tier"] != "blocked" {
		t.Errorf("paused tier = %v, want blocked", dead["tier"])
	}
}

// A rate-limited account is temporarily unusable, and its own snapshot is what
// proves it. The card must say so rather than show the leftover percentages as
// available headroom.
func TestRateLimitedAccountQuotaIsFlagged(t *testing.T) {
	h, store := quotaStatusFixture(t)
	until := time.Now().Add(2 * time.Hour)
	store.MarkRateLimited("claude", "dead@example.com", "", until, false)

	dead := quotaFor(t, statusQuotas(t, h), "dead@example.com")
	if dead["serving"] != false {
		t.Errorf("rate-limited account serving = %v, want false", dead["serving"])
	}
	if dead["account_state"] != "rate_limited" {
		t.Errorf("rate-limited account_state = %v, want rate_limited", dead["account_state"])
	}
	// The distinction that matters: this one fixes itself at a known time, so it
	// must not be lumped in with accounts that need a re-login.
	if dead["tier"] != "waiting" {
		t.Errorf("rate-limited tier = %v, want waiting", dead["tier"])
	}
}

// A snapshot older than the shortest window it reports is describing a past the
// account has already left, so it must be marked instead of read as current.
func TestOldQuotaSnapshotIsMarkedStale(t *testing.T) {
	h, _ := quotaStatusFixture(t)
	old := auth.QuotaCache.Get("claude:dead@example.com")
	old.FetchedTime = time.Now().Add(-13 * 24 * time.Hour)

	cards := statusQuotas(t, h)
	if fresh := quotaFor(t, cards, "live@example.com"); fresh["stale"] != false {
		t.Errorf("just-fetched snapshot marked stale: %v", fresh["stale"])
	}
	stale := quotaFor(t, cards, "dead@example.com")
	if stale["stale"] != true {
		t.Errorf("13-day-old snapshot stale = %v, want true", stale["stale"])
	}
	if age, _ := stale["stale_age"].(string); age != "13d ago" {
		t.Errorf("stale_age = %q, want 13d ago", age)
	}
}

// The three tiers exist so the two ways of "not serving" stay apart: waiting is
// a clock, blocked is a person. quotaTierFor is where that judgement lives.
func TestQuotaTierSeparatesSelfHealingFromHumanAction(t *testing.T) {
	for _, tc := range []struct{ state, want string }{
		{"active", quotaTierServing},
		{"rate_limited", quotaTierWaiting},
		{"revoked", quotaTierBlocked},
		{"disabled", quotaTierBlocked},
		{"expired", quotaTierBlocked},
	} {
		if got := quotaTierFor(tc.state); got != tc.want {
			t.Errorf("quotaTierFor(%q) = %q, want %q", tc.state, got, tc.want)
		}
	}
}
