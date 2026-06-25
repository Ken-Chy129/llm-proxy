package server

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/Ken-Chy129/llm-proxy/internal/auth"
	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/dashboard"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/handler"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
	"github.com/Ken-Chy129/llm-proxy/internal/stats"
)

func Run(configPath string, cfg *config.Config, r *router.Router, tokenStore *auth.TokenStore, keyStore *auth.KeyStore, statsDB *stats.DB,
	claudeOAuth *auth.ClaudeOAuth, codexOAuth *auth.CodexOAuth,
	claudeExec *executor.ClaudeOAuthExecutor, codexExec *executor.CodexExecutor,
	vertexExec *executor.VertexExecutor) error {

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())

	chatHandler := handler.NewChatHandler(r, statsDB)
	adminHandler := handler.NewAdminHandler(configPath, cfg, r, tokenStore, keyStore, statsDB, claudeOAuth, codexOAuth, claudeExec, codexExec, vertexExec)
	imagesHandler := handler.NewImagesHandler(r, statsDB)
	anthropicHandler := handler.NewAnthropicHandler(r, statsDB)

	// Login page and handler (public)
	engine.GET("/login", loginPage())
	engine.POST("/login", loginHandler(cfg))

	// Dashboard (session protected)
	engine.GET("/", SessionAuth(), dashboard.Handler())
	engine.StaticFS("/static", dashboard.StaticFS())

	// /v1/* API routes (Bearer token protected)
	api := engine.Group("/", APIKeyAuth(cfg.Server.APIKey, keyStore), TokenLimitCheck(statsDB))
	responsesHandler := handler.NewResponsesHandler(r, statsDB)

	api.POST("/v1/chat/completions", chatHandler.ChatCompletions)
	api.POST("/v1/responses", responsesHandler.HandleResponses)
	api.POST("/v1/images/generations", imagesHandler.ImagesGenerations)
	api.POST("/v1/images/edits", imagesHandler.ImagesEdits)
	api.POST("/v1/messages", anthropicHandler.Messages)
	api.GET("/v1/models", chatHandler.ListModels)

	// Admin API (session protected, JSON responses)
	admin := engine.Group("/api", SessionAuthJSON())
	admin.GET("/status", adminHandler.Status)
	admin.GET("/logs", adminHandler.Logs)
	admin.GET("/stats", adminHandler.Stats)
	admin.GET("/config", adminHandler.Config)
	admin.PUT("/config", adminHandler.UpdateConfig)
	admin.POST("/sync-models", adminHandler.SyncModels)
	admin.POST("/refresh-quota/:provider/:id", adminHandler.RefreshQuota)
	admin.DELETE("/accounts/:provider/:id", adminHandler.DeleteAccount)
	admin.POST("/vertex/credentials", adminHandler.SetVertexCredentials)
	admin.DELETE("/vertex/credentials", adminHandler.DeleteVertexCredentials)
	admin.POST("/backends/:backend/toggle", adminHandler.ToggleBackend)
	admin.POST("/accounts/:provider/:id/toggle", adminHandler.ToggleAccount)
	admin.GET("/keys", adminHandler.ListKeys)
	admin.POST("/keys", adminHandler.CreateKey)
	admin.PUT("/keys/:id", adminHandler.UpdateKey)
	admin.POST("/keys/:id/toggle", adminHandler.ToggleKey)
	admin.DELETE("/keys/:id", adminHandler.DeleteKey)

	// OAuth login (session protected)
	if claudeOAuth != nil {
		engine.GET("/auth/claude", SessionAuth(), func(c *gin.Context) {
			authURL, err := claudeOAuth.StartLogin()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			if c.Query("json") == "1" {
				c.JSON(http.StatusOK, gin.H{"auth_url": authURL})
				return
			}
			c.Redirect(http.StatusTemporaryRedirect, authURL)
		})
		admin.POST("/auth/claude/exchange", func(c *gin.Context) {
			var req struct {
				CallbackURL string `json:"callback_url"`
			}
			if err := c.ShouldBindJSON(&req); err != nil || req.CallbackURL == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "callback_url is required"})
				return
			}
			token, err := claudeOAuth.ExchangeCallbackURL(c.Request.Context(), req.CallbackURL)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"ok": true, "email": token.Email})
		})
	}
	if codexOAuth != nil {
		engine.GET("/auth/codex", SessionAuth(), func(c *gin.Context) {
			authURL, err := codexOAuth.StartLogin()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			// If requested as JSON (from dashboard modal), return URL; otherwise redirect
			if c.Query("json") == "1" {
				c.JSON(http.StatusOK, gin.H{"auth_url": authURL})
				return
			}
			c.Redirect(http.StatusTemporaryRedirect, authURL)
		})
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
		fmt.Printf("llm-proxy listening on %s (HTTPS)\n", addr)
		return engine.RunTLS(addr, cfg.Server.CertFile, cfg.Server.KeyFile)
	}
	fmt.Printf("llm-proxy listening on %s\n", addr)
	return engine.Run(addr)
}

func loginPage() gin.HandlerFunc {
	return func(c *gin.Context) {
		errMsg := c.Query("error")
		errHTML := ""
		if errMsg != "" {
			errHTML = `<div class="err">! ` + errMsg + `</div>`
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(`<!DOCTYPE html>
<html lang="en"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1.0">
<title>LLM-Proxy · authenticate</title>
<style>
@font-face{font-family:'Chakra Petch';font-weight:600;font-display:swap;src:url('/static/fonts/chakra-petch-600.woff2') format('woff2')}
@font-face{font-family:'Chakra Petch';font-weight:700;font-display:swap;src:url('/static/fonts/chakra-petch-700.woff2') format('woff2')}
@font-face{font-family:'IBM Plex Mono';font-weight:400;font-display:swap;src:url('/static/fonts/ibm-plex-mono-400.woff2') format('woff2')}
@font-face{font-family:'IBM Plex Mono';font-weight:500;font-display:swap;src:url('/static/fonts/ibm-plex-mono-500.woff2') format('woff2')}
:root{--bg-0:#080a09;--bg-1:#0e1110;--bg-3:#1b201d;--border:#242a27;--border-h:#39423d;--text-0:#e9efe9;--text-1:#97a39c;--text-2:#5d665f;--accent:#a3e635;--accent-rgb:163,230,53;--accent-dim:#6f9c24;--red:#ff5f49;--mono:'IBM Plex Mono',ui-monospace,Menlo,monospace;--display:'Chakra Petch',var(--mono)}
*{margin:0;padding:0;box-sizing:border-box}
body{background-color:var(--bg-0);background-image:radial-gradient(900px 500px at 80% -10%,rgba(var(--accent-rgb),0.07),transparent 62%),linear-gradient(rgba(var(--accent-rgb),0.022) 1px,transparent 1px),linear-gradient(90deg,rgba(var(--accent-rgb),0.022) 1px,transparent 1px);background-size:auto,44px 44px,44px 44px;color:var(--text-0);font-family:var(--mono);display:flex;align-items:center;justify-content:center;min-height:100vh;padding:20px}
body::after{content:'';position:fixed;inset:0;pointer-events:none;background:repeating-linear-gradient(0deg,rgba(0,0,0,0.16) 0 1px,transparent 1px 3px),radial-gradient(120% 90% at 50% 0%,transparent 60%,rgba(0,0,0,0.5) 100%);mix-blend-mode:multiply;opacity:.55}
.login-card{position:relative;background:linear-gradient(180deg,var(--bg-1),var(--bg-0));border:1px solid var(--border-h);border-radius:3px;padding:34px 30px;width:380px;max-width:92vw;box-shadow:0 30px 90px -34px #000,0 0 0 1px rgba(var(--accent-rgb),0.05);z-index:1}
.login-card::before{content:'';position:absolute;top:8px;left:8px;width:15px;height:15px;border-top:1px solid var(--accent);border-left:1px solid var(--accent);opacity:.6}
.login-card::after{content:'';position:absolute;bottom:8px;right:8px;width:15px;height:15px;border-bottom:1px solid var(--accent);border-right:1px solid var(--accent);opacity:.6}
.brand{display:flex;align-items:center;gap:11px;margin-bottom:6px}
.led{width:9px;height:9px;border-radius:50%;background:var(--accent);box-shadow:0 0 10px var(--accent),0 0 2px #fff;animation:beat 2.4s ease-in-out infinite}
h2{font-family:var(--display);font-weight:700;font-size:22px;letter-spacing:3px;text-shadow:0 0 18px rgba(var(--accent-rgb),0.25)}
.subtitle{color:var(--text-2);font-size:12px;letter-spacing:1px;margin-bottom:26px}
.subtitle::before{content:'// '}
.err{color:var(--red);font-size:12px;margin-bottom:14px;padding:8px 11px;border:1px solid rgba(255,95,73,0.4);background:rgba(255,95,73,0.1);border-radius:2px;font-family:var(--mono)}
label{display:block;font-family:var(--display);font-size:10px;font-weight:600;letter-spacing:1.5px;text-transform:uppercase;color:var(--text-2);margin-bottom:6px}
input{display:block;width:100%;background:var(--bg-0);border:1px solid var(--border);color:var(--text-0);border-radius:2px;padding:10px 13px;font-size:14px;margin-bottom:18px;font-family:var(--mono)}
input:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 1px var(--accent)}
button{width:100%;padding:12px;margin-top:4px;background:var(--accent);color:#0c110a;border:none;border-radius:2px;font-family:var(--display);font-size:13px;font-weight:600;letter-spacing:2px;text-transform:uppercase;cursor:pointer;box-shadow:0 0 18px -4px rgba(var(--accent-rgb),0.5);transition:all .15s}
button:hover{background:#b6f04f;box-shadow:0 0 26px -2px rgba(var(--accent-rgb),0.7)}
@keyframes beat{0%,100%{box-shadow:0 0 10px var(--accent),0 0 2px #fff;opacity:1}50%{box-shadow:0 0 4px var(--accent);opacity:.6}}
</style></head><body>
<div class="login-card">
<div class="brand"><span class="led"></span><h2>LLM-PROXY</h2></div>
<p class="subtitle">multi-backend console · authenticate</p>
`+errHTML+`
<form method="POST" action="/login">
<label>Username</label>
<input type="text" name="username" required autofocus autocomplete="username">
<label>Password</label>
<input type="password" name="password" required autocomplete="current-password">
<button type="submit">Sign In &rarr;</button>
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
