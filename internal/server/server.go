package server

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/user/cli-proxy/internal/auth"
	"github.com/user/cli-proxy/internal/config"
	"github.com/user/cli-proxy/internal/dashboard"
	"github.com/user/cli-proxy/internal/handler"
	"github.com/user/cli-proxy/internal/router"
	"github.com/user/cli-proxy/internal/stats"
)

func Run(cfg *config.Config, r *router.Router, tokenStore *auth.TokenStore, statsDB *stats.DB, claudeOAuth *auth.ClaudeOAuth, codexOAuth *auth.CodexOAuth) error {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())

	chatHandler := handler.NewChatHandler(r, statsDB)
	adminHandler := handler.NewAdminHandler(cfg, r, tokenStore, statsDB)

	// Dashboard
	engine.GET("/", dashboard.Handler())

	// API routes (protected by api_key if set)
	api := engine.Group("/")
	if cfg.Server.APIKey != "" {
		api.Use(APIKeyAuth(cfg.Server.APIKey))
	}
	api.POST("/v1/chat/completions", chatHandler.ChatCompletions)
	api.GET("/v1/models", chatHandler.ListModels)

	// Admin API (no api_key needed, local access only)
	engine.GET("/api/status", adminHandler.Status)
	engine.GET("/api/logs", adminHandler.Logs)
	engine.GET("/api/stats", adminHandler.Stats)
	engine.GET("/api/config", adminHandler.Config)
	engine.DELETE("/api/accounts/:provider/:id", adminHandler.DeleteAccount)

	// OAuth login routes
	if claudeOAuth != nil {
		engine.GET("/auth/claude", func(c *gin.Context) {
			authURL, err := claudeOAuth.StartLogin()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.Redirect(http.StatusTemporaryRedirect, authURL)
		})
	}
	if codexOAuth != nil {
		engine.GET("/auth/codex", func(c *gin.Context) {
			authURL, err := codexOAuth.StartLogin()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.Redirect(http.StatusTemporaryRedirect, authURL)
		})
	}

	engine.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "models": r.AllModels()})
	})

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	fmt.Printf("cli-proxy listening on %s\n", addr)
	fmt.Printf("dashboard: http://localhost:%d/\n", cfg.Server.Port)
	fmt.Printf("models: %v\n", r.AllModels())
	return engine.Run(addr)
}
