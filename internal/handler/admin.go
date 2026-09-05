package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Ken-Chy129/llm-proxy/internal/auth"
	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
	"github.com/Ken-Chy129/llm-proxy/internal/stats"
	"github.com/gin-gonic/gin"
)

type AdminHandler struct {
	configPath  string
	cfg         *config.Config
	router      *router.Router
	tokenStore  *auth.TokenStore
	keyStore    *auth.KeyStore
	statsDB     *stats.DB
	claudeOAuth *auth.ClaudeOAuth
	codexOAuth  *auth.CodexOAuth
	claudeExec  *executor.ClaudeOAuthExecutor
	codexExec   *executor.CodexExecutor
	vertexExec  *executor.VertexExecutor
	kimiExec    *executor.KimiExecutor
	relayExec   *executor.KimiExecutor
	anygenExec  *executor.AnyGenExecutor
}

func NewAdminHandler(configPath string, cfg *config.Config, r *router.Router, store *auth.TokenStore, keyStore *auth.KeyStore, db *stats.DB, claudeOAuth *auth.ClaudeOAuth, codexOAuth *auth.CodexOAuth, claudeExec *executor.ClaudeOAuthExecutor, codexExec *executor.CodexExecutor, vertexExec *executor.VertexExecutor, kimiExec *executor.KimiExecutor, relayExec *executor.KimiExecutor, anygenExec *executor.AnyGenExecutor) *AdminHandler {
	return &AdminHandler{configPath: configPath, cfg: cfg, router: r, tokenStore: store, keyStore: keyStore, statsDB: db, claudeOAuth: claudeOAuth, codexOAuth: codexOAuth, claudeExec: claudeExec, codexExec: codexExec, vertexExec: vertexExec, kimiExec: kimiExec, relayExec: relayExec, anygenExec: anygenExec}
}

// applyRouting re-runs the shared routing installer after a config edit, so a
// dashboard save and a fresh start produce exactly the same live state.
func (h *AdminHandler) applyRouting() {
	router.Apply(h.router, h.cfg, &router.Providers{
		Claude: h.claudeExec,
		Codex:  h.codexExec,
		Vertex: h.vertexExec,
		Kimi:   h.kimiExec,
		Relay:  h.relayExec,
		AnyGen: h.anygenExec,
	})
}

// formatLocalTime renders a timestamp as HH:MM, prefixing the date (MM-DD) when
// it isn't today, so a rate-limit reset that crosses midnight isn't ambiguous.
func formatLocalTime(t time.Time) string {
	t = t.Local()
	now := time.Now()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	return t.Format("01-02 15:04")
}

func (h *AdminHandler) Status(c *gin.Context) {
	backends := []gin.H{}

	// Vertex — show card even when unconfigured so credentials can be added from the dashboard
	if h.vertexExec != nil {
		vertexDisabled := h.tokenStore.IsBackendDisabled("vertex")
		if h.vertexExec.Configured() {
			source := h.vertexExec.CredentialSource()
			vertexStatus := "active"
			if vertexDisabled {
				vertexStatus = "disabled"
			}
			backends = append(backends, gin.H{
				"name":              "vertex",
				"status":            vertexStatus,
				"disabled":          vertexDisabled,
				"info":              h.vertexExec.ProjectID() + " / " + h.vertexExec.Region() + " · " + source,
				"models":            h.router.ModelsByBackend("vertex"),
				"credential_source": source,
				"catalog":           h.catalogView("vertex"),
			})
		} else {
			backends = append(backends, gin.H{
				"name":    "vertex",
				"status":  "not_authenticated",
				"info":    "No GCP credentials — upload a service account key",
				"models":  h.router.ModelsByBackend("vertex"),
				"catalog": h.catalogView("vertex"),
			})
		}
	}

	// Kimi — API key comes from an environment variable and is never exposed.
	if h.kimiExec != nil && h.cfg.Kimi.Enabled {
		disabled := h.tokenStore.IsBackendDisabled("kimi")
		status := "not_authenticated"
		info := "Missing environment variable " + h.kimiExec.APIKeyEnv()
		if h.kimiExec.Configured() {
			status = "active"
			info = h.kimiExec.BaseURL() + " · " + h.kimiExec.APIFormat() + " · key: " + h.kimiExec.APIKeyEnv()
		}
		if disabled {
			status = "disabled"
		}
		backends = append(backends, gin.H{
			"name":     "kimi",
			"status":   status,
			"disabled": disabled,
			"info":     info,
			"models":   h.router.ModelsByBackend("kimi"),
			"catalog":  h.catalogView("kimi"),
		})
	}

	// Relay — Anthropic-compatible upstream authenticated by an environment token.
	if h.relayExec != nil && h.cfg.Relay.Enabled {
		disabled := h.tokenStore.IsBackendDisabled("relay")
		status := "not_authenticated"
		info := "Missing environment variable " + h.relayExec.APIKeyEnv()
		if h.relayExec.Configured() {
			status = "active"
			info = h.relayExec.BaseURL() + " · anthropic · key: " + h.relayExec.APIKeyEnv()
		}
		if disabled {
			status = "disabled"
		}
		backends = append(backends, gin.H{
			"name":     "relay",
			"status":   status,
			"disabled": disabled,
			"info":     info,
			"models":   h.router.ModelsByBackend("relay"),
			"catalog":  h.catalogView("relay"),
		})
	}

	// AnyGen — models are synced from the free OpenAI-compatible endpoint; the
	// platform-native key verification response supplies the current credits.
	if h.anygenExec != nil && h.cfg.AnyGen.Enabled {
		disabled := h.tokenStore.IsBackendDisabled("anygen")
		status := "not_authenticated"
		info := "Missing environment variable " + h.anygenExec.APIKeyEnv()
		var creditsQuota gin.H
		if h.anygenExec.Configured() {
			status = "active"
			info = "key: " + h.anygenExec.APIKeyEnv()
			if len(h.anygenExec.Models()) == 0 {
				status = "not_authenticated"
				info += " · no models synced"
			}
			if credits, ok := h.anygenExec.Credits(); ok {
				creditsQuota = anyGenCreditsQuota(credits)
				if !credits.Verified {
					status = "not_authenticated"
					info += " · key not verified"
				}
			}
		}
		if disabled {
			status = "disabled"
		}
		entry := gin.H{
			"name":     "anygen",
			"status":   status,
			"disabled": disabled,
			"info":     info,
			"models":   h.router.ModelsByBackend("anygen"),
			"catalog":  h.catalogView("anygen"),
			"endpoint": h.anygenExec.BaseURL(),
		}
		if creditsQuota != nil {
			entry["quotas"] = []gin.H{creditsQuota}
		}
		backends = append(backends, entry)
	}

	// OAuth providers
	allAccounts := h.tokenStore.All()
	for _, p := range []struct {
		name    string
		account string
		enabled bool
	}{
		// The account store keys Claude accounts under "claude" while routing
		// names the provider "claude_oauth"; the two are deliberately separate,
		// since renaming the stored key would orphan every saved token.
		{"claude_oauth", "claude", h.cfg.ClaudeOAuth.Enabled},
		{"codex", "codex", h.cfg.Codex.Enabled},
	} {
		if !p.enabled {
			continue
		}
		accounts := allAccounts[p.account]
		status := "not_authenticated"
		activeCount := 0
		var accountList []gin.H
		for _, t := range accounts {
			info := t.Email
			if info == "" {
				info = t.ID
			}
			accStatus := t.StatusLabel()
			if accStatus == "active" {
				activeCount++
			}
			expireInfo := ""
			if t.ExpiresAt != "" {
				if exp, err := time.Parse(time.RFC3339, t.ExpiresAt); err == nil {
					expireInfo = exp.Format("15:04")
				}
			}
			accDisabled := h.tokenStore.IsAccountDisabled(p.account, t.ID)
			if accDisabled {
				accStatus = "disabled"
			}
			acc := gin.H{
				"id":            t.ID,
				"email":         info,
				"status":        accStatus,
				"expires":       expireInfo,
				"token_expired": t.IsExpired(),
				"disabled":      accDisabled,
			}
			// An account is shown "limited" when it isn't currently selectable:
			// either a reactive 429 cooldown is active, or fresh quota shows its
			// session or all-models-weekly window (Primary/Secondary — never a
			// model-specific limit like Opus/Fable) exhausted with the reset still
			// in the future.
			// "until" is the latest such reset — when the account is usable again.
			// This keeps the badge consistent with the quota card and with what
			// account selection actually does (both key off the same signals).
			now := time.Now()
			var until time.Time
			estimated := false
			if u, est, active := h.tokenStore.RateLimitInfo(p.account, t.ID); active {
				until, estimated = u, est
			}
			if q := auth.QuotaCache.Get(p.account + ":" + t.ID); q != nil && q.HasRealData {
				for _, w := range []*auth.RateWindow{q.Primary, q.Secondary} {
					if w.Exhausted(now) && w.ResetUnix > 0 {
						if r := time.Unix(w.ResetUnix, 0); r.After(until) {
							until, estimated = r, false
						}
					}
				}
			}
			if until.After(now) {
				if !accDisabled {
					accStatus = "rate_limited"
					acc["status"] = accStatus
				}
				acc["rate_limited"] = true
				acc["rate_limited_until"] = formatLocalTime(until)
				acc["rate_limited_estimated"] = estimated
			}
			accountList = append(accountList, acc)
		}
		if activeCount > 0 {
			status = "active"
		} else if len(accounts) > 0 {
			status = "expired"
		}
		info := fmt.Sprintf("%d/%d active", activeCount, len(accounts))
		disabled := h.tokenStore.IsBackendDisabled(p.name)
		if disabled {
			status = "disabled"
		}
		entry := gin.H{
			"name": p.name,
			// The account store keys Claude under "claude" while routing names the
			// provider "claude_oauth". Every account-scoped call (add, pause,
			// remove, quota) has to use this key, not the routing name.
			"account":  p.account,
			"status":   status,
			"info":     info,
			"models":   h.router.ModelsByBackend(p.name),
			"catalog":  h.catalogView(p.name),
			"accounts": accountList,
			"disabled": disabled,
		}
		// Per-account quotas
		if p.account == "claude" || p.account == "codex" {
			var quotas []*auth.QuotaInfo
			for _, a := range accounts {
				if q := auth.QuotaCache.Get(p.account + ":" + a.ID); q != nil {
					quotas = append(quotas, q)
				}
			}
			if len(quotas) > 0 {
				entry["quotas"] = quotas
			}
		}
		backends = append(backends, entry)
	}

	totalReqs, totalTokens, totalCost, _ := h.statsDB.TotalStats()

	// One entry per published model with the provider currently serving it
	// (empty when nothing in its chain can). A model now appears in several
	// backends' lists — it is the chain that names them — so anything that needs
	// "the models this proxy serves" has to read this, not the union of those.
	models := make([]gin.H, 0)
	for _, m := range h.router.AllModels() {
		models = append(models, gin.H{"name": m, "provider": h.router.BackendName(m)})
	}

	c.JSON(http.StatusOK, gin.H{
		"backends":       backends,
		"all_models":     h.router.AllModels(),
		"models":         models,
		"total_requests": totalReqs,
		"total_tokens":   totalTokens,
		"total_cost_usd": totalCost,
	})
}

func (h *AdminHandler) Logs(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	errorsOnly := c.Query("errors") == "1"
	search := c.Query("q")

	logs, total, err := h.statsDB.QueryLogs(limit, offset, errorsOnly, search)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"logs": logs, "total": total})
}

func (h *AdminHandler) Stats(c *gin.Context) {
	rangeParam := c.DefaultQuery("range", "7d")
	days := 7
	switch rangeParam {
	case "today":
		days = 1
	case "7d":
		days = 7
	case "30d":
		days = 30
	case "all":
		days = 3650
	}

	granularity := "day"
	if rangeParam == "today" {
		granularity = "hour"
	}

	// tz: viewer's offset in minutes east of UTC (browser -getTimezoneOffset()).
	// Clamped to the valid real-world range; defaults to UTC.
	tzMinutes, _ := strconv.Atoi(c.DefaultQuery("tz", "0"))
	if tzMinutes < -720 || tzMinutes > 840 {
		tzMinutes = 0
	}

	// Optional global filter: dim (model/key/backend/account) + value. Scopes
	// the series, calendar, summary, and breakdowns — but not the facet lists
	// that populate the filter dropdown.
	filterDim := c.Query("dim")
	filterVal := c.Query("val")
	filterCol := stats.DimensionColumn(filterDim) // "" when dim is empty/unknown → no filter
	if filterCol == "" {
		filterVal = ""
	}

	dims := []string{"model", "key", "backend", "account"}
	// Facets: unfiltered dimension value lists for the dropdown.
	facets := gin.H{}
	facetData := map[string][]stats.DimStats{}
	for _, dim := range dims {
		rows, _ := h.statsDB.StatsByDimension(dim, days, "", "")
		facetData[dim] = rows
		facets[dim] = rows
	}
	// Breakdowns: filtered when a filter is active, else reuse the facets.
	breakdown := map[string][]stats.DimStats{}
	for _, dim := range dims {
		if filterCol != "" {
			rows, _ := h.statsDB.StatsByDimension(dim, days, filterCol, filterVal)
			breakdown[dim] = rows
		} else {
			breakdown[dim] = facetData[dim]
		}
	}

	series, _ := h.statsDB.StatsByBucket(days, tzMinutes, granularity, filterCol, filterVal)
	// Year-long daily buckets for the contribution heatmap (independent of the
	// selected range; the frontend always renders ~52 weeks).
	calendar, _ := h.statsDB.StatsByBucket(366, tzMinutes, "day", filterCol, filterVal)
	summary := h.statsDB.StatsSummary(days, filterCol, filterVal)
	// Error breakdown by HTTP status code (failed rows only).
	byStatus, _ := h.statsDB.StatsByDimension("status", days, filterCol, filterVal)

	c.JSON(http.StatusOK, gin.H{
		"range":       rangeParam,
		"granularity": granularity,
		"filter":      gin.H{"dim": filterDim, "val": filterVal},
		"summary":     summary,
		"series":      series,
		"calendar":    calendar,
		"facets":      facets,
		"by_model":    breakdown["model"],
		"by_key":      breakdown["key"],
		"by_backend":  breakdown["backend"],
		"by_account":  breakdown["account"],
		"by_status":   byStatus,
	})
}

// Pricing returns the per-model price table, plus the served models that have
// no price at all. Rates are published properties of each model rather than
// settings, so the table is read-only; the second list exists because an
// unpriced model is otherwise silently missing from every cost total.
func (h *AdminHandler) Pricing(c *gin.Context) {
	table := h.statsDB.Pricing()
	unpriced := []string{}
	for _, model := range h.router.AllModels() {
		if _, ok := table.Lookup(model); !ok {
			unpriced = append(unpriced, model)
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"models":   table.All(),
		"unpriced": unpriced,
	})
}

func (h *AdminHandler) Config(c *gin.Context) {
	adminUser, _ := h.cfg.AdminCreds()
	c.JSON(http.StatusOK, gin.H{
		"server": gin.H{
			"port":       h.cfg.Port(),
			"admin_user": adminUser,
			// Echoed in full, unlike admin_password: the widget's token has to be
			// copyable from here, and this route already sits behind SessionAuth.
			"tray_token": h.cfg.TrayToken(),
		},
		// The routing table is the model editor's whole subject: one row per
		// published model, each with its ordered providers.
		"models":    h.modelRows(),
		"series":    h.cfg.SeriesDefaults(),
		"providers": h.providerSummaries(),
	})
}

// modelRows renders the routing table for the dashboard, annotating each model
// with what routing would do with it right now: which provider is serving, and
// whether a temporary pin is in force.
func (h *AdminHandler) modelRows() []gin.H {
	routes := h.cfg.Routes()
	out := make([]gin.H, 0, len(routes))
	for _, route := range routes {
		providers := make([]gin.H, 0, len(route.Providers))
		for _, ref := range route.Providers {
			providers = append(providers, gin.H{
				"provider":  ref.Provider,
				"upstream":  ref.Upstream,
				"available": h.providerAvailable(ref.Provider),
			})
		}
		row := gin.H{
			"name":      route.Name,
			"series":    config.SeriesOf(route.Name),
			"providers": providers,
			// Empty when every provider in the chain is unavailable, which is
			// exactly when the model is not being served at all.
			"serving": h.router.BackendName(route.Name),
		}
		if provider, until, ok := h.router.PinnedProvider(route.Name); ok {
			row["pinned"] = gin.H{
				"provider":    provider,
				"until":       until.Unix(),
				"until_local": formatLocalTime(until),
			}
		}
		out = append(out, row)
	}
	return out
}

// providerSummaries describes each provider for the model editor: whether it can
// take traffic, and which models it could serve that are not routed to it yet.
func (h *AdminHandler) providerSummaries() []gin.H {
	out := make([]gin.H, 0, len(config.KnownProviders))
	for _, name := range config.KnownProviders {
		entry := gin.H{
			"name":      name,
			"enabled":   h.cfg.ProviderEnabled(name),
			"available": h.providerAvailable(name),
			"paused":    h.tokenStore.IsBackendDisabled(name),
		}
		if catalog := h.providerCatalog(name); len(catalog) > 0 {
			entry["catalog"] = catalog
		}
		out = append(out, entry)
	}
	return out
}

// providerCatalog lists the models a provider can serve. A catalog pinned in
// config wins over discovery — see Config.ProviderModels for why an operator
// overrides rather than supplements a lying endpoint. Otherwise it is whatever
// the upstream reported at the last sync; providers without a discovery
// endpoint (Vertex) have neither and serve whatever they are told to.
func (h *AdminHandler) providerCatalog(name string) []string {
	if pinned := h.cfg.ProviderModels(name); len(pinned) > 0 {
		return pinned
	}
	switch name {
	case "anygen":
		if h.anygenExec != nil {
			return h.anygenExec.Catalog()
		}
	case "codex":
		if h.codexExec != nil {
			return h.codexExec.Catalog()
		}
	case "kimi":
		if h.kimiExec != nil {
			return h.kimiExec.Catalog()
		}
	case "relay":
		if h.relayExec != nil {
			return h.relayExec.Catalog()
		}
	case "claude_oauth":
		if h.claudeExec != nil {
			return h.claudeExec.Catalog()
		}
	}
	return nil
}

// catalogView reports what a provider's upstream says it can serve, marking the
// entries this proxy has published.
//
// The card is about the provider, so the catalog is the whole of it: a model
// added upstream used to be invisible until someone read release notes, and a
// name that exists only in our routing table (an alias, or one pointing at an
// id the upstream does not have) says nothing about the provider and is left to
// the config page.
//
// Aliases still count as routed, keyed by the upstream id they resolve to —
// "codex-auto-review" published onto gpt-5.6-sol means gpt-5.6-sol is served,
// and offering to publish it again would be wrong.
func (h *AdminHandler) catalogView(provider string) gin.H {
	models := h.providerCatalog(provider)
	if len(models) == 0 {
		return nil
	}
	routed := make(map[string]bool, len(models))
	for _, m := range h.router.ModelsByBackend(provider) {
		routed[m] = true
	}
	// A published model can be renamed upstream (Vertex wants dated ids), and
	// the chain records that mapping — a renamed model is routed, not missing.
	for _, route := range h.cfg.Routes() {
		for _, ref := range route.Providers {
			if ref.Provider == provider && strings.TrimSpace(ref.Upstream) != "" {
				routed[ref.Upstream] = true
			}
		}
	}

	list := make([]gin.H, 0, len(models))
	unrouted := 0
	for _, id := range models {
		isRouted := routed[id]
		if !isRouted {
			unrouted++
		}
		list = append(list, gin.H{"id": id, "routed": isRouted})
	}
	return gin.H{
		"models":   list,
		"total":    len(list),
		"unrouted": unrouted,
	}
}

// providerAvailable reports whether a provider could serve a request right now:
// enabled in config, not paused, and holding whatever credentials it needs.
func (h *AdminHandler) providerAvailable(name string) bool {
	if !h.cfg.ProviderEnabled(name) || h.tokenStore.IsBackendDisabled(name) {
		return false
	}
	switch name {
	case "claude_oauth":
		return h.claudeExec != nil && len(h.tokenStore.AllForProvider("claude")) > 0
	case "codex":
		return h.codexExec != nil && len(h.tokenStore.AllForProvider("codex")) > 0
	case "vertex":
		return h.vertexExec != nil && h.vertexExec.Configured()
	case "kimi":
		return h.kimiExec != nil && h.kimiExec.Configured()
	case "relay":
		return h.relayExec != nil && h.relayExec.Configured()
	case "anygen":
		return h.anygenExec != nil && h.anygenExec.Configured()
	}
	return false
}

// cleanModelList trims, drops empty entries, and rejects duplicates.
func cleanModelList(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if seen[m] {
			return nil, fmt.Errorf("duplicate model %q", m)
		}
		seen[m] = true
		out = append(out, m)
	}
	return out, nil
}

// UpdateConfig edits the routing table and server settings. Routing edits apply
// live by re-running the same function startup uses; server settings are
// persisted to the config file, with port requiring a restart to take effect
// (admin credentials apply live because loginHandler reads cfg on every
// request).
func (h *AdminHandler) UpdateConfig(c *gin.Context) {
	// Every section is a pointer, so an absent one means "leave it alone". The
	// dashboard saves Models and Admin independently — with value structs,
	// saving the admin password would send an empty model table and wipe routing.
	var req struct {
		Models *[]config.ModelRoute `json:"models"`
		Series *config.SeriesConfig `json:"series"`
		Server *struct {
			Port          int    `json:"port"`
			AdminUser     string `json:"admin_user"`
			AdminPassword string `json:"admin_password"`
			// Pointer, not string: this field is prefilled in the dashboard, so an
			// empty string is a deliberate revoke. Absent (nil) must still mean
			// "leave it alone", otherwise any client that PUTs a config without
			// this key silently locks out the desktop widget.
			TrayToken *string `json:"tray_token"`
		} `json:"server"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}

	// --- validate everything before touching anything ---
	series := h.cfg.SeriesDefaults()
	if req.Series != nil {
		series = *req.Series
	}
	routes := h.cfg.Routes()
	if req.Models != nil {
		normalized, err := config.NormalizeRoutes(*req.Models, series)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		routes = normalized
	}
	// Port is optional: 0 means "leave unchanged". When provided it must be valid.
	if req.Server != nil && req.Server.Port != 0 && (req.Server.Port < 1 || req.Server.Port > 65535) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "server port must be between 1 and 65535"})
		return
	}

	// --- detect restart-required changes before mutating cfg ---
	var restart []string
	if req.Server != nil && req.Server.Port != 0 && req.Server.Port != h.cfg.Port() {
		restart = append(restart, "port")
	}

	if req.Server != nil {
		h.cfg.SetAdminCreds(req.Server.AdminUser, req.Server.AdminPassword)
		if req.Server.TrayToken != nil {
			h.cfg.SetTrayToken(strings.TrimSpace(*req.Server.TrayToken))
		}
		if req.Server.Port != 0 {
			h.cfg.SetPort(req.Server.Port)
		}
	}

	// --- update in-memory cfg, apply live, then persist ---
	if req.Series != nil {
		h.cfg.SetSeries(series)
	}
	if req.Models != nil || req.Series != nil {
		// Already validated; the setter owns the config lock that concurrent
		// GET /api/config and atomic config saves need.
		if err := h.cfg.SetRoutes(routes); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		h.applyRouting()
	}

	if err := config.Save(h.configPath, h.cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save config: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "restart_required": restart})
}

func (h *AdminHandler) SyncModels(c *gin.Context) {
	results := gin.H{}

	if h.codexOAuth != nil {
		models, _, err := h.codexOAuth.FetchModels(c.Request.Context())
		if err != nil {
			results["codex"] = gin.H{"error": err.Error()}
		} else {
			slugs := make([]string, len(models))
			for i, m := range models {
				slugs[i] = m.Slug
			}
			// Without this the sync reported a fresh list while the dashboard
			// kept rendering the catalog discovered at startup.
			if h.codexExec != nil {
				h.codexExec.SetCatalog(slugs)
			}
			results["codex"] = gin.H{"models": slugs, "count": len(slugs)}
		}
		// Refresh all account quotas
		h.codexOAuth.FetchAllQuotas(c.Request.Context())
	}
	if h.claudeOAuth != nil {
		h.claudeOAuth.FetchAllQuotas(c.Request.Context())
		entry := gin.H{"quotas": "refreshed"}
		if models, err := h.claudeOAuth.FetchModels(c.Request.Context()); err != nil {
			entry["error"] = err.Error()
		} else {
			if h.claudeExec != nil {
				h.claudeExec.SetCatalog(models)
			}
			entry["models"] = models
			entry["count"] = len(models)
		}
		results["claude"] = entry
	}
	if h.anygenExec != nil && h.cfg.AnyGen.Enabled && h.anygenExec.Configured() {
		models, err := h.anygenExec.SyncModels(c.Request.Context())
		if err != nil {
			results["anygen"] = gin.H{"error": err.Error()}
		} else {
			results["anygen"] = gin.H{"models": models, "count": len(models)}
		}
	}
	// The API-key upstreams answer /v1/models too, so they are discoverable on
	// the same button. A failure is reported per provider rather than aborting:
	// one relay being down must not hide a fresh Codex catalog.
	for _, keyed := range []struct {
		name    string
		enabled bool
		exec    *executor.KimiExecutor
	}{
		{"kimi", h.cfg.Kimi.Enabled, h.kimiExec},
		{"relay", h.cfg.Relay.Enabled, h.relayExec},
	} {
		if keyed.exec == nil || !keyed.enabled || !keyed.exec.Configured() {
			continue
		}
		models, err := keyed.exec.SyncModels(c.Request.Context())
		if err != nil {
			results[keyed.name] = gin.H{"error": err.Error()}
			continue
		}
		results[keyed.name] = gin.H{"models": models, "count": len(models)}
	}
	// A sync changes what each provider *could* serve; routing decides what it
	// does serve, so re-apply it rather than registering anything directly.
	h.applyRouting()

	c.JSON(http.StatusOK, results)
}

// PublishModel adds one discovered model to the routing table.
//
// This is the counterpart to the catalog on each provider card: seeing that an
// upstream gained a model is only useful if acting on it does not mean hand-
// editing config.yaml. The new route is seeded from the model's series default
// so it inherits the same failover chain its siblings use, with the discovering
// provider guaranteed a place in it — publishing from AnyGen's card must not
// produce a chain that never reaches AnyGen.
//
// It writes config.yaml and re-applies routing through the same path a
// dashboard save uses, so a published model is servable immediately and the
// running state cannot drift from the file.
func (h *AdminHandler) PublishModel(c *gin.Context) {
	var req struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	provider := strings.TrimSpace(req.Provider)
	model := strings.TrimSpace(req.Model)
	if provider == "" || model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider and model are required"})
		return
	}
	if !slices.Contains(config.KnownProviders, provider) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown provider " + provider})
		return
	}
	// Publishing a name that is already published would silently rewrite that
	// model's chain, which is an edit the operator did not ask for.
	routes := h.cfg.Routes()
	for _, route := range routes {
		if route.Name == model {
			c.JSON(http.StatusConflict, gin.H{"error": model + " is already published"})
			return
		}
	}

	chain := h.publishChain(provider, model)
	routes = append(routes, config.ModelRoute{Name: model, Providers: chain})
	if err := h.cfg.SetRoutes(routes); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := config.Save(h.configPath, h.cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save config: " + err.Error()})
		return
	}
	h.applyRouting()

	providers := make([]string, len(chain))
	for i, ref := range chain {
		providers[i] = ref.Provider
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":        true,
		"model":     model,
		"providers": providers,
		"serving":   h.router.BackendName(model),
	})
}

// publishChain picks the provider chain a newly published model starts with.
//
// The series default is the right starting point — it is what every other model
// in the family uses — but only if the provider the model was discovered on can
// actually be reached through it. A gemini model found on AnyGen must not be
// published onto a claude chain that never lists AnyGen, so the discovering
// provider is appended when the default omits it, and is the whole chain when
// there is no default at all.
func (h *AdminHandler) publishChain(provider, model string) []config.ProviderRef {
	defaults := h.cfg.SeriesDefaults()[config.SeriesOf(model)]
	chain := make([]config.ProviderRef, 0, len(defaults)+1)
	for _, ref := range defaults {
		// Only providers that could serve this model belong in the seeded chain;
		// a default naming a provider whose catalog lacks the model would add a
		// hop that always fails over.
		if ref.Provider == provider || h.providerOffers(ref.Provider, model) {
			chain = append(chain, config.ProviderRef{Provider: ref.Provider})
		}
	}
	if !slices.ContainsFunc(chain, func(ref config.ProviderRef) bool { return ref.Provider == provider }) {
		chain = append(chain, config.ProviderRef{Provider: provider})
	}
	return chain
}

// providerOffers reports whether a provider's discovered catalog contains a
// model. Providers without a catalog (Vertex, Claude OAuth) answer true: they
// have no discovery endpoint, so absence is unknown rather than "no".
func (h *AdminHandler) providerOffers(provider, model string) bool {
	catalog := h.providerCatalog(provider)
	if len(catalog) == 0 {
		return true
	}
	return slices.Contains(catalog, model)
}

func (h *AdminHandler) RefreshQuota(c *gin.Context) {
	provider := c.Param("provider")
	id := c.Param("id")
	if provider == "anygen" && h.anygenExec != nil {
		credits, err := h.anygenExec.RefreshCredits(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "quota": anyGenCreditsQuota(credits)})
		return
	}
	if provider == "codex" && h.codexOAuth != nil {
		if err := h.codexOAuth.FetchQuotaForAccountByID(c.Request.Context(), id); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		q := auth.QuotaCache.Get("codex:" + id)
		c.JSON(http.StatusOK, gin.H{"ok": true, "quota": q})
		return
	}
	if provider == "claude" && h.claudeOAuth != nil {
		if err := h.claudeOAuth.FetchQuotaForAccountByID(c.Request.Context(), id); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		q := auth.QuotaCache.Get("claude:" + id)
		c.JSON(http.StatusOK, gin.H{"ok": true, "quota": q})
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported provider"})
}

func anyGenCreditsQuota(credits executor.AnyGenCredits) gin.H {
	return gin.H{
		"kind":          "credits",
		"account_id":    "anygen",
		"display_name":  "Platform balance",
		"plan_type":     "Credits",
		"has_real_data": credits.Verified && strings.TrimSpace(credits.Credits) != "",
		"verified":      credits.Verified,
		"credits":       credits.Credits,
	}
}

// SetVertexCredentials accepts an uploaded GCP credential JSON from the
// dashboard, verifies it by fetching a token, persists it, and (re)registers
// the vertex backend without a restart.
func (h *AdminHandler) SetVertexCredentials(c *gin.Context) {
	if h.vertexExec == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "vertex executor not available"})
		return
	}
	var req struct {
		CredentialsJSON string `json:"credentials_json"`
		ProjectID       string `json:"project_id"`
		Region          string `json:"region"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.CredentialsJSON) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "credentials_json is required"})
		return
	}
	credsJSON := []byte(strings.TrimSpace(req.CredentialsJSON))
	if err := h.vertexExec.ApplyCredentials(c.Request.Context(), req.ProjectID, req.Region, credsJSON, true); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := auth.SaveGCPCredential(h.tokenStore.Dir(), &auth.GCPCredential{
		ProjectID:   h.vertexExec.ProjectID(),
		Region:      h.vertexExec.Region(),
		Credentials: json.RawMessage(credsJSON),
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save credentials: " + err.Error()})
		return
	}
	h.applyRouting()
	c.JSON(http.StatusOK, gin.H{
		"ok":         true,
		"project_id": h.vertexExec.ProjectID(),
		"region":     h.vertexExec.Region(),
		"models":     h.vertexExec.Models(),
	})
}

// DeleteVertexCredentials removes uploaded credentials. Falls back to ADC if
// the config file still defines a project, otherwise unregisters the backend.
func (h *AdminHandler) DeleteVertexCredentials(c *gin.Context) {
	if h.vertexExec == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "vertex executor not available"})
		return
	}
	if err := auth.DeleteGCPCredential(h.tokenStore.Dir()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	stillConfigured := h.vertexExec.ClearCredentials()
	h.applyRouting()
	c.JSON(http.StatusOK, gin.H{"ok": true, "configured": stillConfigured})
}

func (h *AdminHandler) DeleteAccount(c *gin.Context) {
	provider := c.Param("provider")
	id := c.Param("id")
	if err := h.tokenStore.Remove(provider, id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *AdminHandler) ToggleBackend(c *gin.Context) {
	backend := c.Param("backend")
	if h.tokenStore.IsBackendDisabled(backend) {
		h.tokenStore.EnableBackend(backend)
		c.JSON(http.StatusOK, gin.H{"ok": true, "backend": backend, "disabled": false})
	} else {
		h.tokenStore.DisableBackend(backend)
		c.JSON(http.StatusOK, gin.H{"ok": true, "backend": backend, "disabled": true})
	}
}

// PinModel temporarily forces one model onto one provider.
//
// The pin lives in memory and expires, which is the point: trying a provider
// out should not rewrite config.yaml, and an experiment someone forgets about
// must not survive a restart. Editing the model's chain is how a lasting change
// is made.
func (h *AdminHandler) PinModel(c *gin.Context) {
	var req struct {
		Model      string `json:"model"`
		Provider   string `json:"provider"`
		TTLMinutes int    `json:"ttl_minutes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	// An empty provider clears the pin, which is what the "revert" control sends.
	if strings.TrimSpace(req.Provider) == "" {
		_ = h.router.Pin(req.Model, "", 0)
		c.JSON(http.StatusOK, gin.H{"ok": true, "pinned": false})
		return
	}
	ttl := time.Duration(req.TTLMinutes) * time.Minute
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	if err := h.router.Pin(req.Model, req.Provider, ttl); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	_, until, _ := h.router.PinnedProvider(req.Model)
	c.JSON(http.StatusOK, gin.H{
		"ok":          true,
		"pinned":      true,
		"provider":    req.Provider,
		"until":       until.Unix(),
		"until_local": formatLocalTime(until),
	})
}

func (h *AdminHandler) ListKeys(c *gin.Context) {
	// tz: viewer's offset in minutes east of UTC, so "today" matches the tray
	// widget's local-calendar-day figures instead of drifting by the UTC offset.
	tzMinutes, _ := strconv.Atoi(c.DefaultQuery("tz", "0"))

	keys := h.keyStore.All()
	keyStats, _ := h.statsDB.StatsByKey(tzMinutes)
	statsMap := make(map[string]*stats.KeyStats)
	for i := range keyStats {
		statsMap[keyStats[i].KeyName] = &keyStats[i]
	}

	result := make([]gin.H, len(keys))
	for i, k := range keys {
		entry := gin.H{
			"id":                  k.ID,
			"name":                k.Name,
			"key":                 k.Key,
			"token_limit_daily":   k.TokenLimitDaily,
			"request_limit_daily": k.RequestLimitDaily,
			"created_at":          k.CreatedAt,
			"disabled":            k.Disabled,
		}
		if s := statsMap[k.Name]; s != nil {
			entry["request_count"] = s.RequestCount
			entry["total_tokens"] = s.TotalTokens
			entry["tokens_today"] = s.TokensToday
			entry["requests_today"] = s.RequestsToday
			entry["error_count"] = s.ErrorCount
			// The figure token_limit_daily is actually enforced against — smaller
			// than tokens_today, which includes cache. The UI grades the limit by
			// this one and displays the other.
			entry["quota_used_today"] = s.QuotaUsedToday
		}
		result[i] = entry
	}
	c.JSON(http.StatusOK, gin.H{"keys": result})
}

func (h *AdminHandler) CreateKey(c *gin.Context) {
	var req struct {
		Name              string `json:"name"`
		TokenLimitDaily   int    `json:"token_limit_daily"`
		RequestLimitDaily int    `json:"request_limit_daily"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	kd, err := h.keyStore.Add(req.Name, req.TokenLimitDaily, req.RequestLimitDaily)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "key": kd})
}

func (h *AdminHandler) UpdateKey(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Name              string `json:"name"`
		TokenLimitDaily   int    `json:"token_limit_daily"`
		RequestLimitDaily int    `json:"request_limit_daily"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.keyStore.Update(id, req.Name, req.TokenLimitDaily, req.RequestLimitDaily); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *AdminHandler) DeleteKey(c *gin.Context) {
	id := c.Param("id")
	if err := h.keyStore.Delete(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *AdminHandler) ToggleKey(c *gin.Context) {
	id := c.Param("id")
	var cur *auth.KeyData
	for _, k := range h.keyStore.All() {
		if k.ID == id {
			cur = k
			break
		}
	}
	if cur == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "key not found"})
		return
	}
	target := !cur.Disabled // capture before SetDisabled mutates the shared pointer
	if err := h.keyStore.SetDisabled(id, target); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "disabled": target})
}

func (h *AdminHandler) ToggleAccount(c *gin.Context) {
	provider := c.Param("provider")
	id := c.Param("id")
	if h.tokenStore.IsAccountDisabled(provider, id) {
		h.tokenStore.EnableAccount(provider, id)
		c.JSON(http.StatusOK, gin.H{"ok": true, "disabled": false})
	} else {
		h.tokenStore.DisableAccount(provider, id)
		c.JSON(http.StatusOK, gin.H{"ok": true, "disabled": true})
	}
}
