package main

import (
	"context"
	"flag"
	"log"
	"time"

	"github.com/Ken-Chy129/llm-proxy/internal/auth"
	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
	"github.com/Ken-Chy129/llm-proxy/internal/server"
	"github.com/Ken-Chy129/llm-proxy/internal/stats"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()
	log.Println("[deploy-check] redeploy round-trip marker")

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	r := router.New()
	tokenStore := auth.NewTokenStore(cfg.ClaudeOAuth.TokenDir)
	r.SetChecker(tokenStore)
	auth.InitQuotaCache(tokenStore.Dir())

	statsDB, err := stats.Open(cfg.ClaudeOAuth.TokenDir)
	if err != nil {
		log.Fatalf("open stats db: %v", err)
	}
	defer statsDB.Close()

	// Vertex: configured via config file (ADC) or dashboard-uploaded credentials
	vertexExec := executor.NewVertexExecutor(cfg.Vertex)
	if saved := auth.LoadGCPCredential(tokenStore.Dir()); saved != nil {
		if err := vertexExec.ApplyCredentials(context.Background(), saved.ProjectID, saved.Region, saved.Credentials, false); err != nil {
			log.Printf("apply saved gcp credentials: %v", err)
		} else {
			log.Printf("loaded uploaded gcp credentials (project=%s)", vertexExec.ProjectID())
		}
	}
	if vertexExec.Configured() {
		r.Register(vertexExec, "vertex")
		log.Printf("registered vertex executor: %v (project=%s, source=%s)",
			vertexExec.Models(), vertexExec.ProjectID(), vertexExec.CredentialSource())
	}

	// Claude OAuth: static models (Claude doesn't expose a model list API)
	var claudeOAuth *auth.ClaudeOAuth
	var claudeExec *executor.ClaudeOAuthExecutor
	if cfg.ClaudeOAuth.Enabled {
		claudeOAuth = auth.NewClaudeOAuth(tokenStore)
		claudeOAuth.ServerPort = cfg.Server.Port
		models := cfg.ClaudeOAuth.Models
		if len(models) == 0 {
			models = []string{"claude-sonnet-4-6", "claude-opus-4-6"}
		}
		claudeExec = executor.NewClaudeOAuthExecutor(claudeOAuth, models)
		r.Register(claudeExec, "claude")
		log.Printf("registered claude oauth executor: %v", models)
	}

	// Codex OAuth: try to dynamically fetch models, fall back to config
	var codexOAuth *auth.CodexOAuth
	var codexExec *executor.CodexExecutor
	if cfg.Codex.Enabled {
		codexOAuth = auth.NewCodexOAuth(tokenStore)
		codexOAuth.ServerPort = cfg.Server.Port
		// Start with config models
		models := cfg.Codex.Models
		codexExec = executor.NewCodexExecutor(codexOAuth, models)
		r.Register(codexExec, "codex")

		// Refresh all tokens at startup, then fetch models
		if len(tokenStore.AllForProvider("codex")) > 0 {
			codexOAuth.RefreshAllTokens(context.Background())
			syncCodexModels(codexOAuth, codexExec, r)
			// Fetch quota for all accounts (warmup + /codex/usage)
			log.Printf("fetching codex quotas for %d accounts...", len(tokenStore.AllForProvider("codex")))
			codexOAuth.FetchAllQuotas(context.Background())
		}
	}

	keyStore := auth.NewKeyStore(tokenStore.Dir())

	// Cleanup old logs (retain 90 days), run at startup and daily
	if deleted, err := statsDB.Cleanup(90); err == nil && deleted > 0 {
		log.Printf("cleaned up %d old log entries", deleted)
	}
	go func() {
		for range time.NewTicker(24 * time.Hour).C {
			if d, err := statsDB.Cleanup(90); err == nil && d > 0 {
				log.Printf("daily cleanup: removed %d old entries", d)
			}
		}
	}()

	if err := server.Run(*configPath, cfg, r, tokenStore, keyStore, statsDB, claudeOAuth, codexOAuth, claudeExec, codexExec, vertexExec); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func syncCodexModels(oauth *auth.CodexOAuth, exec *executor.CodexExecutor, r *router.Router) {
	models, _, err := oauth.FetchModels(context.Background())
	if err != nil {
		log.Printf("failed to fetch codex models: %v", err)
		return
	}
	r.UnregisterBackend("codex")
	for _, m := range models {
		r.RegisterModel(m.Slug, exec, "codex")
	}
	slugs := make([]string, len(models))
	for i, m := range models {
		slugs[i] = m.Slug
	}
	log.Printf("synced %d codex models: %v", len(models), slugs)

	// Quota is fetched separately via FetchAllQuotas
}
