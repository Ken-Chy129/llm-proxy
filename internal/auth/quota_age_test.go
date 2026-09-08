package auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// FetchedAt carries no year, so reloading a cache used to parse every snapshot
// back to year zero — which made fresh readings look infinitely old. The epoch
// is persisted alongside it so age survives a restart.
func TestQuotaCacheAgeSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	InitQuotaCache(dir)
	QuotaCache.Set("claude:a@example.com", &QuotaInfo{
		AccountID: "a@example.com", HasRealData: true,
		Primary: &RateWindow{Label: "Current session (5h)", RemainingPercent: 100},
	})

	InitQuotaCache(dir)
	got := QuotaCache.Get("claude:a@example.com")
	if got == nil {
		t.Fatal("snapshot did not survive reload")
	}
	age, ok := got.Age(time.Now())
	if !ok {
		t.Fatal("reloaded snapshot reports no age")
	}
	if age > time.Minute {
		t.Errorf("reloaded snapshot aged %s, want ~0", age)
	}
	if QuotaCache.IsStale("claude:a@example.com", time.Hour) {
		t.Error("a snapshot written seconds ago is reported stale after reload")
	}
}

// A cache file written before the epoch existed has no usable timestamp. Age
// must say so rather than invent one.
func TestQuotaAgeUnknownWithoutTimestamp(t *testing.T) {
	if _, ok := (&QuotaInfo{FetchedAt: "01/02 15:04"}).Age(time.Now()); ok {
		t.Error("a snapshot with no epoch reported a definite age")
	}
}

// A cache file from before the epoch field must not be resurrected with a
// year-zero timestamp: that is what made every reloaded card look ancient.
func TestLegacyQuotaCacheFileHasNoFabricatedAge(t *testing.T) {
	dir := t.TempDir()
	legacy := `[{"key":"claude:a@example.com","info":{"account_id":"a@example.com","fetched_at":"09/08 17:22","has_real_data":true}}]`
	if err := os.WriteFile(filepath.Join(dir, "quota_cache.json"), []byte(legacy), 0600); err != nil {
		t.Fatalf("write legacy cache: %v", err)
	}

	InitQuotaCache(dir)
	got := QuotaCache.Get("claude:a@example.com")
	if got == nil {
		t.Fatal("legacy snapshot did not load")
	}
	if age, ok := got.Age(time.Now()); ok {
		t.Errorf("legacy snapshot claimed a definite age of %s", age)
	}
	if !got.FetchedTime.IsZero() {
		t.Errorf("legacy snapshot got a fabricated timestamp %s", got.FetchedTime)
	}
}
