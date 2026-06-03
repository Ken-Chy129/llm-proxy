package handler

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/user/cli-proxy/internal/auth"
	"github.com/user/cli-proxy/internal/config"
	"github.com/user/cli-proxy/internal/router"
	"github.com/user/cli-proxy/internal/stats"
)

type AdminHandler struct {
	cfg        *config.Config
	router     *router.Router
	tokenStore *auth.TokenStore
	statsDB    *stats.DB
	codexOAuth *auth.CodexOAuth
}

func NewAdminHandler(cfg *config.Config, r *router.Router, store *auth.TokenStore, db *stats.DB, codexOAuth *auth.CodexOAuth) *AdminHandler {
	return &AdminHandler{cfg: cfg, router: r, tokenStore: store, statsDB: db, codexOAuth: codexOAuth}
}

func (h *AdminHandler) Status(c *gin.Context) {
	backends := []gin.H{}

	// Vertex — always "active" if configured
	if h.cfg.Vertex.ProjectID != "" {
		models := make([]string, len(h.cfg.Vertex.Models))
		for i, m := range h.cfg.Vertex.Models {
			models[i] = m.Name
		}
		backends = append(backends, gin.H{
			"name":    "vertex",
			"status":  "active",
			"info":    h.cfg.Vertex.ProjectID + " / " + h.cfg.Vertex.Region,
			"models":  models,
		})
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
		// Add quota for codex
		if p.name == "codex" && h.codexOAuth != nil && activeCount > 0 {
			quota, err := h.codexOAuth.FetchQuota(context.Background())
			if err != nil {
				entry["quota_error"] = err.Error()
			} else {
				entry["quota"] = quota
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
		models, err := h.codexOAuth.FetchModels(c.Request.Context())
		if err != nil {
			results["codex"] = gin.H{"error": err.Error()}
		} else {
			slugs := make([]string, len(models))
			for i, m := range models {
				slugs[i] = m.Slug
			}
			results["codex"] = gin.H{"models": slugs, "count": len(slugs)}
		}
	}

	c.JSON(http.StatusOK, results)
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
