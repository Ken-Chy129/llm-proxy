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
	imagesHandler := handler.NewImagesHandler(r, statsDB)

	// Login page and handler (public)
	engine.GET("/login", loginPage())
	engine.POST("/login", loginHandler(cfg))

	// Dashboard (session protected)
	engine.GET("/", SessionAuth(), dashboard.Handler())

	// /v1/* API routes (Bearer token protected)
	api := engine.Group("/", APIKeyAuth(cfg.Server.APIKey))
	responsesHandler := handler.NewResponsesHandler(r, statsDB)

	api.POST("/v1/chat/completions", chatHandler.ChatCompletions)
	api.POST("/v1/responses", responsesHandler.HandleResponses)
	api.POST("/v1/images/generations", imagesHandler.ImagesGenerations)
	api.GET("/v1/models", chatHandler.ListModels)

	// Admin API (session protected, JSON responses)
	admin := engine.Group("/api", SessionAuthJSON())
	admin.GET("/status", adminHandler.Status)
	admin.GET("/logs", adminHandler.Logs)
	admin.GET("/stats", adminHandler.Stats)
	admin.GET("/config", adminHandler.Config)
	admin.POST("/sync-models", adminHandler.SyncModels)
	admin.POST("/refresh-quota/:provider/:id", adminHandler.RefreshQuota)
	admin.DELETE("/accounts/:provider/:id", adminHandler.DeleteAccount)

	// OAuth login (session protected)
	if claudeOAuth != nil {
		engine.GET("/auth/claude", SessionAuth(), func(c *gin.Context) {
			authURL, err := claudeOAuth.StartLogin()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.Redirect(http.StatusTemporaryRedirect, authURL)
		})
	}
	if codexOAuth != nil {
		engine.GET("/auth/codex", SessionAuth(), func(c *gin.Context) {
			authURL, err := codexOAuth.StartLogin()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.Redirect(http.StatusTemporaryRedirect, authURL)
		})
		// Exchange callback URL (paste method)
		admin.POST("/auth/codex/exchange", func(c *gin.Context) {
			var req struct {
				CallbackURL string `json:"callback_url"`
			}
			if err := c.ShouldBindJSON(&req); err != nil || req.CallbackURL == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "callback_url is required"})
				return
			}
			token, err := codexOAuth.ExchangeCallbackURL(c.Request.Context(), req.CallbackURL)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"ok": true, "email": token.Email})
		})
	}

	// Health (public)
	engine.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	fmt.Printf("models: %v\n", r.AllModels())
	if cfg.Server.CertFile != "" && cfg.Server.KeyFile != "" {
		fmt.Printf("cli-proxy listening on %s (HTTPS)\n", addr)
		return engine.RunTLS(addr, cfg.Server.CertFile, cfg.Server.KeyFile)
	}
	fmt.Printf("cli-proxy listening on %s\n", addr)
	return engine.Run(addr)
}

func loginPage() gin.HandlerFunc {
	return func(c *gin.Context) {
		errMsg := c.Query("error")
		errHTML := ""
		if errMsg != "" {
			errHTML = `<div style="color:var(--red);font-size:13px;margin-bottom:12px">` + errMsg + `</div>`
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(`<!DOCTYPE html>
<html><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1.0">
<title>CLI Proxy - Login</title>
<style>
:root{--bg-0:#0a0a0f;--bg-1:#12121a;--border:#2a2a3e;--text-0:#eeeef2;--text-2:#707088;--accent:#5b8aff;--accent-dim:#3a5ccc;--red:#f87171}
*{margin:0;padding:0;box-sizing:border-box}
body{background:var(--bg-0);color:var(--text-0);font-family:-apple-system,sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh}
.login-card{background:var(--bg-1);border:1px solid var(--border);border-radius:12px;padding:32px;width:360px}
h2{font-size:18px;font-weight:600;margin-bottom:6px}
.subtitle{color:var(--text-2);font-size:13px;margin-bottom:24px}
label{display:block;font-size:12px;color:var(--text-2);margin-bottom:4px}
input{display:block;width:100%;background:var(--bg-0);border:1px solid var(--border);color:var(--text-0);border-radius:6px;padding:8px 12px;font-size:14px;margin-bottom:16px;font-family:inherit}
input:focus{outline:none;border-color:var(--accent)}
button{width:100%;padding:10px;background:var(--accent);color:#fff;border:none;border-radius:6px;font-size:14px;font-weight:500;cursor:pointer}
button:hover{background:var(--accent-dim)}
</style></head><body>
<div class="login-card">
<h2>CLI Proxy</h2>
<p class="subtitle">Sign in to access the dashboard</p>
`+errHTML+`
<form method="POST" action="/login">
<label>Username</label>
<input type="text" name="username" required autofocus>
<label>Password</label>
<input type="password" name="password" required>
<button type="submit">Sign In</button>
</form>
</div></body></html>`))
	}
}

func loginHandler(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		username := c.PostForm("username")
		password := c.PostForm("password")

		if username == cfg.Server.AdminUser && password == cfg.Server.AdminPassword {
			token := sessions.Create()
			secure := cfg.Server.CertFile != ""
			c.SetCookie("session", token, 86400*7, "/", "", secure, true)
			c.Redirect(http.StatusFound, "/")
			return
		}

		c.Redirect(http.StatusFound, "/login?error=Invalid+username+or+password")
	}
}
