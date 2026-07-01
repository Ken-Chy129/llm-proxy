package auth

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type QuotaInfo struct {
	AccountID   string         `json:"account_id"`
	Email       string         `json:"email,omitempty"`
	PlanType    string         `json:"plan_type,omitempty"`
	Primary     *RateWindow    `json:"primary,omitempty"`
	Secondary   *RateWindow    `json:"secondary,omitempty"`
	Additional  []AdditionalRL `json:"additional,omitempty"`
	Credits     *Credits       `json:"credits,omitempty"`
	FetchedAt   string         `json:"fetched_at,omitempty"`
	FetchedTime time.Time      `json:"-"`
	HasRealData bool           `json:"has_real_data"`
}

type RateWindow struct {
	Label            string  `json:"label"`
	RemainingPercent float64 `json:"remaining_percent"`
	LimitReached     bool    `json:"limit_reached"`
	ResetAt          string  `json:"reset_at,omitempty"`
	// ResetUnix is the machine-readable reset time (unix seconds) behind ResetAt.
	// Used by quota-aware account selection to compare reset times; 0 = unknown.
	ResetUnix int64 `json:"reset_unix,omitempty"`
}

// Exhausted reports whether this window is currently used up: the limit was
// reached and its reset time is still in the future (or unknown). Once the
// reset time has passed, the window is treated as fresh again even if a stale
// snapshot still says LimitReached — so selection and UI recover at the reset
// boundary without waiting for the next quota fetch. Nil is never exhausted.
func (w *RateWindow) Exhausted(now time.Time) bool {
	if w == nil || !w.LimitReached {
		return false
	}
	return w.ResetUnix <= 0 || time.Unix(w.ResetUnix, 0).After(now)
}

type AdditionalRL struct {
	Name    string     `json:"name"`
	Primary *RateWindow `json:"primary,omitempty"`
}

type Credits struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance,omitempty"`
}

type ModelInfo struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	Description string `json:"description,omitempty"`
}

// QuotaCache stores per-account quota info, persisted to disk.
var QuotaCache *quotaCache

func InitQuotaCache(dir string) {
	QuotaCache = &quotaCache{data: make(map[string]*QuotaInfo), dir: dir}
	QuotaCache.load()
}

type quotaCache struct {
	mu   sync.RWMutex
	data map[string]*QuotaInfo // key: "provider:accountID"
	dir  string
}

func (c *quotaCache) Get(key string) *QuotaInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.data[key]
}

func (c *quotaCache) Set(key string, info *QuotaInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	info.FetchedTime = time.Now()
	info.FetchedAt = time.Now().Format("01/02 15:04")
	c.data[key] = info
	c.persist()
}

func (c *quotaCache) IsStale(key string, maxAge time.Duration) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	info, ok := c.data[key]
	if !ok || !info.HasRealData {
		return true
	}
	return time.Since(info.FetchedTime) > maxAge
}

func (c *quotaCache) AllForProvider(provider string) []*QuotaInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var result []*QuotaInfo
	for _, v := range c.data {
		if v != nil {
			result = append(result, v)
		}
	}
	return result
}

func (c *quotaCache) All() map[string]*QuotaInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cp := make(map[string]*QuotaInfo, len(c.data))
	for k, v := range c.data {
		cp[k] = v
	}
	return cp
}

func (c *quotaCache) persist() {
	if c.dir == "" {
		return
	}
	type persistEntry struct {
		Key  string     `json:"key"`
		Info *QuotaInfo `json:"info"`
	}
	var entries []persistEntry
	for k, v := range c.data {
		entries = append(entries, persistEntry{Key: k, Info: v})
	}
	raw, _ := json.MarshalIndent(entries, "", "  ")
	os.WriteFile(filepath.Join(c.dir, "quota_cache.json"), raw, 0600)
}

func (c *quotaCache) load() {
	if c.dir == "" {
		return
	}
	raw, err := os.ReadFile(filepath.Join(c.dir, "quota_cache.json"))
	if err != nil {
		return
	}
	type persistEntry struct {
		Key  string     `json:"key"`
		Info *QuotaInfo `json:"info"`
	}
	var entries []persistEntry
	if json.Unmarshal(raw, &entries) != nil {
		return
	}
	for _, e := range entries {
		if e.Info != nil {
			if e.Info.FetchedAt != "" {
				e.Info.FetchedTime, _ = time.Parse("01/02 15:04", e.Info.FetchedAt)
			}
			c.data[e.Key] = e.Info
		}
	}
}

func formatResetAt(resetAtUnix float64) string {
	if resetAtUnix <= 0 {
		return ""
	}
	t := time.Unix(int64(resetAtUnix), 0)
	return t.Format("01/02 15:04")
}

func windowLabel(windowMinutes int) string {
	if windowMinutes <= 0 {
		return "限额"
	}
	hours := windowMinutes / 60
	if hours < 24 {
		return strconv.Itoa(hours) + " 小时限额"
	}
	days := hours / 24
	if days == 7 {
		return "周限额"
	}
	return strconv.Itoa(days) + " 天限额"
}

// ParseCodexRateLimitHeaders extracts quota from Codex response headers.
func ParseCodexRateLimitHeaders(h http.Header) *QuotaInfo {
	pctStr := h.Get("x-codex-primary-used-percent")
	if pctStr == "" {
		return nil
	}
	pct, err := strconv.ParseFloat(pctStr, 64)
	if err != nil {
		return nil
	}

	info := &QuotaInfo{FetchedAt: time.Now().Format("01/02 15:04"), HasRealData: true}

	winMin := 300
	if v := h.Get("x-codex-primary-window-minutes"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			winMin = n
		}
	}
	resetAt := ""
	var resetUnix int64
	if v := h.Get("x-codex-primary-reset-at"); v != "" {
		if n, e := strconv.ParseFloat(v, 64); e == nil {
			resetAt = formatResetAt(n)
			resetUnix = int64(n)
		}
	}
	remaining := math.Max(0, math.Round((100-pct)*100)/100)
	info.Primary = &RateWindow{
		Label:            windowLabel(winMin),
		RemainingPercent: remaining,
		LimitReached:     pct >= 100,
		ResetAt:          resetAt,
		ResetUnix:        resetUnix,
	}

	if secPctStr := h.Get("x-codex-secondary-used-percent"); secPctStr != "" {
		if secPct, e := strconv.ParseFloat(secPctStr, 64); e == nil {
			secWinMin := 10080
			if v := h.Get("x-codex-secondary-window-minutes"); v != "" {
				if n, e := strconv.Atoi(v); e == nil {
					secWinMin = n
				}
			}
			secReset := ""
			var secResetUnix int64
			if v := h.Get("x-codex-secondary-reset-at"); v != "" {
				if n, e := strconv.ParseFloat(v, 64); e == nil {
					secReset = formatResetAt(n)
					secResetUnix = int64(n)
				}
			}
			secRemaining := math.Max(0, math.Round((100-secPct)*100)/100)
			info.Secondary = &RateWindow{
				Label:            windowLabel(secWinMin),
				RemainingPercent: secRemaining,
				LimitReached:     secPct >= 100,
				ResetAt:          secReset,
				ResetUnix:        secResetUnix,
			}
		}
	}

	if h.Get("x-codex-credits-has-credits") == "true" {
		info.Credits = &Credits{
			HasCredits: true,
			Unlimited:  h.Get("x-codex-credits-unlimited") == "true",
			Balance:    h.Get("x-codex-credits-balance"),
		}
	}

	return info
}

// RateLimitResetTime inspects a 429 response and returns when the account should
// be retried, plus whether that time came from an authoritative upstream hint
// (known=true) or is a fallback guess (known=false). It checks, in order:
// Retry-After (seconds or HTTP-date), anthropic-ratelimit-unified-reset (unix
// seconds), and x-codex-primary-reset-at (unix seconds). If none parse to a
// future time it returns now+defaultCooldown with known=false.
//
// Note: Anthropic's Claude-OAuth 429s often carry only `x-should-retry: true`
// and no reset header at all, so for that backend known is usually false and the
// cooldown is an estimate.
func RateLimitResetTime(h http.Header, defaultCooldown time.Duration) (time.Time, bool) {
	now := time.Now()
	if v := h.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return now.Add(time.Duration(secs) * time.Second), true
		}
		if t, err := http.ParseTime(v); err == nil && t.After(now) {
			return t, true
		}
	}
	for _, key := range []string{"anthropic-ratelimit-unified-reset", "x-codex-primary-reset-at"} {
		if v := h.Get(key); v != "" {
			if ts, err := strconv.ParseFloat(v, 64); err == nil {
				if t := time.Unix(int64(ts), 0); t.After(now) {
					return t, true
				}
			}
		}
	}
	return now.Add(defaultCooldown), false
}
