package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/Ken-Chy129/llm-proxy/internal/auth"
	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
	"github.com/Ken-Chy129/llm-proxy/internal/stats"
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
}

func NewAdminHandler(configPath string, cfg *config.Config, r *router.Router, store *auth.TokenStore, keyStore *auth.KeyStore, db *stats.DB, claudeOAuth *auth.ClaudeOAuth, codexOAuth *auth.CodexOAuth, claudeExec *executor.ClaudeOAuthExecutor, codexExec *executor.CodexExecutor, vertexExec *executor.VertexExecutor) *AdminHandler {
	return &AdminHandler{configPath: configPath, cfg: cfg, router: r, tokenStore: store, keyStore: keyStore, statsDB: db, claudeOAuth: claudeOAuth, codexOAuth: codexOAuth, claudeExec: claudeExec, codexExec: codexExec, vertexExec: vertexExec}
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
				"models":            h.vertexExec.Models(),
				"credential_source": source,
			})
		} else {
			backends = append(backends, gin.H{
				"name":   "vertex",
				"status": "not_authenticated",
				"info":   "No GCP credentials — upload a service account key",
				"models": h.vertexExec.Models(),
			})
		}
	}

	// OAuth providers
	allAccounts := h.tokenStore.All()
	for _, p := range []struct {
		name    string
		enabled bool
		models  []string
	}{
		{"claude", h.cfg.ClaudeOAuth.Enabled, h.cfg.ClaudeOAuth.Models},
		{"codex", h.cfg.Codex.Enabled, h.cfg.Codex.Models},
	} {
		if !p.enabled {
			continue
		}
		accounts := allAccounts[p.name]
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
			accDisabled := h.tokenStore.IsAccountDisabled(p.name, t.ID)
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
			if until, estimated, active := h.tokenStore.RateLimitInfo(p.name, t.ID); active {
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
		// Use dynamic models from router instead of config
		dynamicModels := h.router.ModelsByBackend(p.name)
		if len(dynamicModels) == 0 {
			dynamicModels = p.models
		}
		disabled := h.tokenStore.IsBackendDisabled(p.name)
		if disabled {
			status = "disabled"
		}
		entry := gin.H{
			"name":     p.name,
			"status":   status,
			"info":     info,
			"models":   dynamicModels,
			"accounts": accountList,
			"disabled": disabled,
		}
		// Per-account quotas
		if p.name == "codex" {
			var quotas []*auth.QuotaInfo
			for _, a := range accounts {
				if q := auth.QuotaCache.Get("codex:" + a.ID); q != nil {
					quotas = append(quotas, q)
				}
			}
			if len(quotas) > 0 {
				entry["quotas"] = quotas
			}
		}
		backends = append(backends, entry)
	}

	totalReqs, totalTokens, _ := h.statsDB.TotalStats()

	c.JSON(http.StatusOK, gin.H{
		"backends":     backends,
		"all_models":   h.router.AllModels(),
		"total_requests": totalReqs,
		"total_tokens":   totalTokens,
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
	reqs, toks, errs, avgLat := h.statsDB.StatsSummary(days, filterCol, filterVal)
	// Error breakdown by HTTP status code (failed rows only).
	byStatus, _ := h.statsDB.StatsByDimension("status", days, filterCol, filterVal)

	c.JSON(http.StatusOK, gin.H{
		"range":       rangeParam,
		"granularity": granularity,
		"filter":      gin.H{"dim": filterDim, "val": filterVal},
		"summary":     gin.H{"requests": reqs, "tokens": toks, "errors": errs, "avg_latency_ms": avgLat},
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

func (h *AdminHandler) Config(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"server": gin.H{
			"port":       h.cfg.Server.Port,
			"admin_user": h.cfg.Server.AdminUser,
		},
		"vertex": gin.H{
			"project_id": h.cfg.Vertex.ProjectID,
			"region":     h.cfg.Vertex.Region,
			"models":     h.cfg.Vertex.Models,
		},
		"claude_oauth": gin.H{
			"enabled": h.cfg.ClaudeOAuth.Enabled,
			"models":  h.cfg.ClaudeOAuth.Models,
		},
		"codex": gin.H{
			"enabled": h.cfg.Codex.Enabled,
			"models":  h.cfg.Codex.Models,
		},
	})
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

// UpdateConfig edits the net-new config surface (model lists + server settings)
// not already controllable from the BACKENDS tab. Model-list edits apply live by
// re-registering the executors with the router; server settings are persisted to
// the config file, with port requiring a restart to take effect (admin
// credentials apply live because loginHandler reads cfg on every request).
func (h *AdminHandler) UpdateConfig(c *gin.Context) {
	var req struct {
		ClaudeOAuth struct {
			Models []string `json:"models"`
		} `json:"claude_oauth"`
		Codex struct {
			Models []string `json:"models"`
		} `json:"codex"`
		Vertex struct {
			Models []config.ModelConfig `json:"models"`
		} `json:"vertex"`
		Server struct {
			Port          int    `json:"port"`
			AdminUser     string `json:"admin_user"`
			AdminPassword string `json:"admin_password"`
		} `json:"server"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}

	// --- validate ---
	claudeModels, err := cleanModelList(req.ClaudeOAuth.Models)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "claude models: " + err.Error()})
		return
	}
	codexModels, err := cleanModelList(req.Codex.Models)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "codex models: " + err.Error()})
		return
	}
	vertexModels := make([]config.ModelConfig, 0, len(req.Vertex.Models))
	seenAlias := make(map[string]bool)
	for _, m := range req.Vertex.Models {
		name := strings.TrimSpace(m.Name)
		model := strings.TrimSpace(m.Model)
		if name == "" && model == "" {
			continue
		}
		if name == "" || model == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "vertex models: each row needs both an alias and a model"})
			return
		}
		if seenAlias[name] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "vertex models: duplicate alias " + name})
			return
		}
		seenAlias[name] = true
		vertexModels = append(vertexModels, config.ModelConfig{Name: name, Model: model})
	}
	// Port is optional: 0 means "leave unchanged". When provided it must be valid.
	if req.Server.Port != 0 && (req.Server.Port < 1 || req.Server.Port > 65535) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "server port must be between 1 and 65535"})
		return
	}

	// --- detect restart-required changes before mutating cfg ---
	var restart []string
	if req.Server.Port != 0 && req.Server.Port != h.cfg.Server.Port {
		restart = append(restart, "port")
	}

	// --- apply live ---
	if h.claudeExec != nil && h.cfg.ClaudeOAuth.Enabled {
		h.claudeExec.SetModels(claudeModels)
		h.router.UnregisterBackend("claude")
		h.router.Register(h.claudeExec, "claude")
	}
	if h.vertexExec != nil {
		h.vertexExec.SetModels(vertexModels)
		if h.vertexExec.Configured() {
			h.router.UnregisterBackend("vertex")
			h.router.Register(h.vertexExec, "vertex")
		}
	}
	if req.Server.AdminUser != "" {
		h.cfg.Server.AdminUser = req.Server.AdminUser
	}
	if req.Server.AdminPassword != "" {
		h.cfg.Server.AdminPassword = req.Server.AdminPassword
	}

	// --- update in-memory cfg, then persist ---
	h.cfg.ClaudeOAuth.Models = claudeModels
	h.cfg.Codex.Models = codexModels
	h.cfg.Vertex.Models = vertexModels
	if req.Server.Port != 0 {
		h.cfg.Server.Port = req.Server.Port
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
			results["codex"] = gin.H{"models": slugs, "count": len(slugs)}
		}
		// Refresh all account quotas
		h.codexOAuth.FetchAllQuotas(c.Request.Context())
	}

	c.JSON(http.StatusOK, results)
}

func (h *AdminHandler) RefreshQuota(c *gin.Context) {
	provider := c.Param("provider")
	id := c.Param("id")
	if provider == "codex" && h.codexOAuth != nil {
		if err := h.codexOAuth.FetchQuotaForAccountByID(c.Request.Context(), id); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		q := auth.QuotaCache.Get("codex:" + id)
		c.JSON(http.StatusOK, gin.H{"ok": true, "quota": q})
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported provider"})
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
	h.router.UnregisterBackend("vertex")
	h.router.Register(h.vertexExec, "vertex")
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
	if !stillConfigured {
		h.router.UnregisterBackend("vertex")
	}
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

func (h *AdminHandler) ListKeys(c *gin.Context) {
	keys := h.keyStore.All()
	keyStats, _ := h.statsDB.StatsByKey()
	statsMap := make(map[string]*stats.KeyStats)
	for i := range keyStats {
		statsMap[keyStats[i].KeyName] = &keyStats[i]
	}

	result := make([]gin.H, len(keys))
	for i, k := range keys {
		entry := gin.H{
			"id":                k.ID,
			"name":              k.Name,
			"key":               k.Key,
			"token_limit_daily": k.TokenLimitDaily,
			"created_at":        k.CreatedAt,
			"disabled":          k.Disabled,
		}
		if s := statsMap[k.Name]; s != nil {
			entry["request_count"] = s.RequestCount
			entry["total_tokens"] = s.TotalTokens
			entry["tokens_today"] = s.TokensToday
			entry["error_count"] = s.ErrorCount
		}
		result[i] = entry
	}
	c.JSON(http.StatusOK, gin.H{"keys": result})
}

func (h *AdminHandler) CreateKey(c *gin.Context) {
	var req struct {
		Name            string `json:"name"`
		TokenLimitDaily int    `json:"token_limit_daily"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	kd, err := h.keyStore.Add(req.Name, req.TokenLimitDaily)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "key": kd})
}

func (h *AdminHandler) UpdateKey(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Name            string `json:"name"`
		TokenLimitDaily int    `json:"token_limit_daily"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.keyStore.Update(id, req.Name, req.TokenLimitDaily); err != nil {
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
