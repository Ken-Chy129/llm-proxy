package auth

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type QuotaInfo struct {
	AccountID   string         `json:"account_id"`
	PlanType    string         `json:"plan_type,omitempty"`
	Primary     *RateWindow    `json:"primary,omitempty"`
	Secondary   *RateWindow    `json:"secondary,omitempty"`
	Additional  []AdditionalRL `json:"additional,omitempty"`
	Credits     *Credits       `json:"credits,omitempty"`
	FetchedAt   string         `json:"fetched_at,omitempty"`
}

type RateWindow struct {
	Label        string  `json:"label"`
	UsedPercent  float64 `json:"used_percent"`
	LimitReached bool    `json:"limit_reached"`
	ResetAt      string  `json:"reset_at,omitempty"`
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

// QuotaCache stores per-account quota info.
var QuotaCache = &quotaCache{data: make(map[string]*QuotaInfo)}

type quotaCache struct {
	mu   sync.RWMutex
	data map[string]*QuotaInfo // key: "provider:accountID"
}

func (c *quotaCache) Get(key string) *QuotaInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.data[key]
}

func (c *quotaCache) Set(key string, info *QuotaInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[key] = info
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

	info := &QuotaInfo{FetchedAt: time.Now().Format("01/02 15:04")}

	winMin := 300
	if v := h.Get("x-codex-primary-window-minutes"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			winMin = n
		}
	}
	resetAt := ""
	if v := h.Get("x-codex-primary-reset-at"); v != "" {
		if n, e := strconv.ParseFloat(v, 64); e == nil {
			resetAt = formatResetAt(n)
		}
	}
	info.Primary = &RateWindow{
		Label:        windowLabel(winMin),
		UsedPercent:  math.Round(pct*100) / 100,
		LimitReached: pct >= 100,
		ResetAt:      resetAt,
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
			if v := h.Get("x-codex-secondary-reset-at"); v != "" {
				if n, e := strconv.ParseFloat(v, 64); e == nil {
					secReset = formatResetAt(n)
				}
			}
			info.Secondary = &RateWindow{
				Label:        windowLabel(secWinMin),
				UsedPercent:  math.Round(secPct*100) / 100,
				LimitReached: secPct >= 100,
				ResetAt:      secReset,
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
