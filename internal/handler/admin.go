package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/user/cli-proxy/internal/auth"
	"github.com/user/cli-proxy/internal/config"
	"github.com/user/cli-proxy/internal/executor"
	"github.com/user/cli-proxy/internal/router"
	"github.com/user/cli-proxy/internal/stats"
)

type AdminHandler struct {
	cfg        *config.Config
	router     *router.Router
	tokenStore *auth.TokenStore
	statsDB    *stats.DB
	codexOAuth *auth.CodexOAuth
	vertexExec *executor.VertexExecutor
}

func NewAdminHandler(cfg *config.Config, r *router.Router, store *auth.TokenStore, db *stats.DB, codexOAuth *auth.CodexOAuth, vertexExec *executor.VertexExecutor) *AdminHandler {
	return &AdminHandler{cfg: cfg, router: r, tokenStore: store, statsDB: db, codexOAuth: codexOAuth, vertexExec: vertexExec}
}

func (h *AdminHandler) Status(c *gin.Context) {
	backends := []gin.H{}

	// Vertex — show card even when unconfigured so credentials can be added from the dashboard
	if h.vertexExec != nil {
		if h.vertexExec.Configured() {
			source := h.vertexExec.CredentialSource()
			backends = append(backends, gin.H{
				"name":              "vertex",
				"status":            "active",
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
			accountList = append(accountList, gin.H{
				"id":      t.ID,
				"email":   info,
				"status":  accStatus,
				"expires": expireInfo,
			})
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
		entry := gin.H{
			"name":     p.name,
			"status":   status,
			"info":     info,
			"models":   dynamicModels,
			"accounts": accountList,
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

	logs, total, err := h.statsDB.QueryLogs(limit, offset)
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

	byModel, _ := h.statsDB.StatsByModel(days)
	byDay, _ := h.statsDB.StatsByDay(days)

	c.JSON(http.StatusOK, gin.H{
		"range":    rangeParam,
		"by_model": byModel,
		"by_day":   byDay,
	})
}

func (h *AdminHandler) Config(c *gin.Context) {
	apiKey := ""
	if h.cfg.Server.APIKey != "" {
		apiKey = h.cfg.Server.APIKey[:4] + strings.Repeat("*", len(h.cfg.Server.APIKey)-4)
	}
	c.JSON(http.StatusOK, gin.H{
		"server": gin.H{
			"port":    h.cfg.Server.Port,
			"api_key": apiKey,
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
