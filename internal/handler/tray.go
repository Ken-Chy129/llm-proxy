package handler

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/Ken-Chy129/llm-proxy/internal/auth"
	"github.com/Ken-Chy129/llm-proxy/internal/stats"
	"github.com/gin-gonic/gin"
)

// Tray serves a compact snapshot for the desktop tray widget: per-account quota
// headroom plus today's usage and how it compares to recent days.
//
// It deliberately does NOT reuse Status/Stats: those return large payloads
// (full model lists, 366-day calendars, four dimension breakdowns) and Stats'
// "today" is a rolling 24h window rather than a calendar day. A tray polling
// every 30s needs a small response and a calendar-day figure.
type trayAccount struct {
	Provider string `json:"provider"`
	Email    string `json:"email"`
	PlanType string `json:"plan_type,omitempty"`
	Status   string `json:"status"`

	// SessionPercent / WeeklyPercent are remaining headroom (0-100), or nil when
	// no real quota data has been fetched yet — the tray must distinguish
	// "0% left" from "unknown", which a bare 0 would conflate.
	SessionPercent *float64 `json:"session_percent,omitempty"`
	WeeklyPercent  *float64 `json:"weekly_percent,omitempty"`
	// Both reset times are "MM/DD HH:MM" in server-local time, empty when the
	// upstream quota payload did not carry one.
	//
	// The *Unix twins exist because the widget shows a countdown ("42m until
	// capacity returns"), and deriving that from "07/31 16:50" means guessing the
	// year and re-deriving the timezone — which breaks across new year and DST.
	// Send the instant; let the client format it.
	SessionResetAt   string `json:"session_reset_at,omitempty"`
	SessionResetUnix int64  `json:"session_reset_unix,omitempty"`
	WeeklyResetAt    string `json:"weekly_reset_at,omitempty"`
	WeeklyResetUnix  int64  `json:"weekly_reset_unix,omitempty"`

	RateLimited      bool   `json:"rate_limited,omitempty"`
	RateLimitedUntil string `json:"rate_limited_until,omitempty"`
	QuotaFetchedAt   string `json:"quota_fetched_at,omitempty"`
	HasRealData      bool   `json:"has_real_data"`
}

type trayResponse struct {
	GeneratedAt string `json:"generated_at"`

	// Worst remaining headroom across all usable accounts — what the tray title
	// shows at a glance. Nil when no account has real quota data.
	MinSessionPercent *float64 `json:"min_session_percent,omitempty"`
	MinWeeklyPercent  *float64 `json:"min_weekly_percent,omitempty"`

	Accounts []trayAccount `json:"accounts"`

	Today     stats.TrayDayUsage `json:"today"`
	Yesterday stats.TrayDayUsage `json:"yesterday"`

	Avg7dRequests float64 `json:"avg_7d_requests"`
	Avg7dTokens   float64 `json:"avg_7d_tokens"`

	HourlyTokens []int `json:"hourly_tokens"`

	LastRequestAt  string `json:"last_request_at,omitempty"`
	IdleSeconds    int64  `json:"idle_seconds,omitempty"`
	BackendsActive int    `json:"backends_active"`
	BackendsTotal  int    `json:"backends_total"`
}

func (h *AdminHandler) Tray(c *gin.Context) {
	tzMinutes, _ := strconv.Atoi(c.DefaultQuery("tz", "0"))
	if tzMinutes < -720 || tzMinutes > 840 {
		tzMinutes = 0
	}

	now := time.Now()
	resp := trayResponse{
		GeneratedAt:  now.Format(time.RFC3339),
		Accounts:     []trayAccount{},
		HourlyTokens: make([]int, 24),
	}

	allAccounts := h.tokenStore.All()
	for _, provider := range []struct {
		name    string
		enabled bool
	}{
		{"claude", h.cfg.ClaudeOAuth.Enabled},
		{"codex", h.cfg.Codex.Enabled},
	} {
		if !provider.enabled {
			continue
		}
		for _, t := range allAccounts[provider.name] {
			email := t.Email
			if email == "" {
				email = t.ID
			}
			acc := trayAccount{
				Provider: provider.name,
				Email:    email,
				Status:   t.StatusLabel(),
			}
			if h.tokenStore.IsAccountDisabled(provider.name, t.ID) {
				acc.Status = "disabled"
			}

			// Mirror Status's rate-limit logic: a reactive 429 cooldown or an
			// exhausted session/weekly window whose reset is still ahead.
			var until time.Time
			if u, _, active := h.tokenStore.RateLimitInfo(provider.name, t.ID); active {
				until = u
			}

			if q := auth.QuotaCache.Get(provider.name + ":" + t.ID); q != nil {
				acc.PlanType = q.PlanType
				acc.QuotaFetchedAt = q.FetchedAt
				acc.HasRealData = q.HasRealData
				if q.HasRealData {
					if q.Primary != nil {
						v := q.Primary.RemainingPercent
						acc.SessionPercent = &v
						acc.SessionResetAt = q.Primary.ResetAt
						acc.SessionResetUnix = q.Primary.ResetUnix
					}
					if q.Secondary != nil {
						v := q.Secondary.RemainingPercent
						acc.WeeklyPercent = &v
						acc.WeeklyResetAt = q.Secondary.ResetAt
						acc.WeeklyResetUnix = q.Secondary.ResetUnix
					}
					for _, w := range []*auth.RateWindow{q.Primary, q.Secondary} {
						if w.Exhausted(now) && w.ResetUnix > 0 {
							if r := time.Unix(w.ResetUnix, 0); r.After(until) {
								until = r
							}
						}
					}
				}
			}

			if until.After(now) {
				acc.RateLimited = true
				acc.RateLimitedUntil = formatLocalTime(until)
				if acc.Status != "disabled" {
					acc.Status = "rate_limited"
				}
			}
			resp.Accounts = append(resp.Accounts, acc)
		}
	}

	// Aggregate the worst headroom across accounts that could actually serve
	// traffic — a disabled or expired account's leftover quota is misleading.
	for _, a := range resp.Accounts {
		if a.Status == "disabled" || !a.HasRealData {
			continue
		}
		if a.SessionPercent != nil {
			if resp.MinSessionPercent == nil || *a.SessionPercent < *resp.MinSessionPercent {
				v := *a.SessionPercent
				resp.MinSessionPercent = &v
			}
		}
		if a.WeeklyPercent != nil {
			if resp.MinWeeklyPercent == nil || *a.WeeklyPercent < *resp.MinWeeklyPercent {
				v := *a.WeeklyPercent
				resp.MinWeeklyPercent = &v
			}
		}
	}
	sort.SliceStable(resp.Accounts, func(i, j int) bool {
		return resp.Accounts[i].Email < resp.Accounts[j].Email
	})

	if today, err := h.statsDB.DayUsage(0, tzMinutes); err == nil {
		resp.Today = today
	}
	if yday, err := h.statsDB.DayUsage(1, tzMinutes); err == nil {
		resp.Yesterday = yday
	}
	if ar, at, err := h.statsDB.DailyAverage(7, tzMinutes); err == nil {
		resp.Avg7dRequests, resp.Avg7dTokens = ar, at
	}
	if hourly, err := h.statsDB.HourlyToday(tzMinutes); err == nil {
		resp.HourlyTokens = hourly
	}
	if last, ok := h.statsDB.LastRequestTime(); ok {
		resp.LastRequestAt = last.Local().Format(time.RFC3339)
		if idle := now.Sub(last); idle > 0 {
			resp.IdleSeconds = int64(idle.Seconds())
		}
	}

	// Backend health, so the tray can flag a wholly dead backend.
	for _, name := range []string{"claude", "codex", "kimi", "vertex"} {
		configured := false
		switch name {
		case "claude":
			configured = h.cfg.ClaudeOAuth.Enabled
		case "codex":
			configured = h.cfg.Codex.Enabled
		case "kimi":
			configured = h.kimiExec != nil && h.cfg.Kimi.Enabled && h.kimiExec.Configured()
		case "vertex":
			configured = h.vertexExec != nil && h.vertexExec.Configured()
		}
		if !configured {
			continue
		}
		resp.BackendsTotal++
		if !h.tokenStore.IsBackendDisabled(name) {
			resp.BackendsActive++
		}
	}

	c.JSON(http.StatusOK, resp)
}
