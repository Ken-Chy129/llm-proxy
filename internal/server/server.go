package server

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/user/cli-proxy/internal/auth"
	"github.com/user/cli-proxy/internal/config"
	"github.com/user/cli-proxy/internal/dashboard"
	"github.com/user/cli-proxy/internal/executor"
	"github.com/user/cli-proxy/internal/handler"
	"github.com/user/cli-proxy/internal/router"
	"github.com/user/cli-proxy/internal/stats"
)

func Run(cfg *config.Config, r *router.Router, tokenStore *auth.TokenStore, statsDB *stats.DB,
	claudeOAuth *auth.ClaudeOAuth, codexOAuth *auth.CodexOAuth,
	claudeExec *executor.ClaudeOAuthExecutor, codexExec *executor.CodexExecutor) error {

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())

	chatHandler := handler.NewChatHandler(r, statsDB)
	adminHandler := handler.NewAdminHandler(cfg, r, tokenStore, statsDB, codexOAuth)

	engine.GET("/", dashboard.Handler())

	api := engine.Group("/")
	if cfg.Server.APIKey != "" {
		api.Use(APIKeyAuth(cfg.Server.APIKey))
	}
	imagesHandler := handler.NewImagesHandler(r, statsDB)

	api.POST("/v1/chat/completions", chatHandler.ChatCompletions)
	api.POST("/v1/images/generations", imagesHandler.ImagesGenerations)
	api.GET("/v1/models", chatHandler.ListModels)

	engine.GET("/api/status", adminHandler.Status)
	engine.GET("/api/logs", adminHandler.Logs)
	engine.GET("/api/stats", adminHandler.Stats)
	engine.GET("/api/config", adminHandler.Config)
	engine.POST("/api/sync-models", adminHandler.SyncModels)
	engine.POST("/api/refresh-quota/:provider/:id", adminHandler.RefreshQuota)
	engine.DELETE("/api/accounts/:provider/:id", adminHandler.DeleteAccount)

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
