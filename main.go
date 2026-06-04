package main

import (
	"context"
	"flag"
	"log"

	"github.com/user/cli-proxy/internal/auth"
	"github.com/user/cli-proxy/internal/config"
	"github.com/user/cli-proxy/internal/executor"
	"github.com/user/cli-proxy/internal/router"
	"github.com/user/cli-proxy/internal/server"
	"github.com/user/cli-proxy/internal/stats"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	r := router.New()
	tokenStore := auth.NewTokenStore(cfg.ClaudeOAuth.TokenDir)

	statsDB, err := stats.Open(cfg.ClaudeOAuth.TokenDir)
	if err != nil {
		log.Fatalf("open stats db: %v", err)
	}
	defer statsDB.Close()

	// Vertex: static models from config
	if cfg.Vertex.ProjectID != "" {
		vertexExec := executor.NewVertexExecutor(cfg.Vertex)
		r.Register(vertexExec, "vertex")
		log.Printf("registered vertex executor: %v", vertexExec.Models())
	}

	// Claude OAuth: static models (Claude doesn't expose a model list API)
	var claudeOAuth *auth.ClaudeOAuth
	var claudeExec *executor.ClaudeOAuthExecutor
	if cfg.ClaudeOAuth.Enabled {
		claudeOAuth = auth.NewClaudeOAuth(tokenStore)
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
		// Start with config models
		models := cfg.Codex.Models
		codexExec = executor.NewCodexExecutor(codexOAuth, models)
		r.Register(codexExec, "codex")

		// Try dynamic fetch if already authenticated
		if tokenStore.ActiveCount("codex") > 0 {
			syncCodexModels(codexOAuth, codexExec, r)
		}

		// Seed plan_type from stored tokens if no quota cached yet
		if auth.QuotaCache.Get("codex") == nil {
			for _, t := range tokenStore.AllForProvider("codex") {
				if pt := auth.ParseJWTPlanType(t.AccessToken); pt != "" {
					auth.QuotaCache.Set("codex", &auth.QuotaInfo{
						PlanType:  pt,
						RateLimit: &auth.RateLimit{Allowed: true},
					})
					log.Printf("codex plan (from token): %s", pt)
					break
				}
			}
		}
	}

	if err := server.Run(cfg, r, tokenStore, statsDB, claudeOAuth, codexOAuth, claudeExec, codexExec); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func syncCodexModels(oauth *auth.CodexOAuth, exec *executor.CodexExecutor, r *router.Router) {
	models, client, err := oauth.FetchModels(context.Background())
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

	// Fetch quota using the same warmed client (shares CF cookies)
	if client != nil {
		quota, err := oauth.FetchQuotaWithClient(context.Background(), client)
		if err != nil {
			log.Printf("failed to fetch codex quota: %v", err)
		} else {
			auth.QuotaCache.Set("codex", quota)
			log.Printf("codex quota: plan=%s used=%.0f%% limit_reached=%v", quota.PlanType, quota.RateLimit.UsedPercent, quota.RateLimit.LimitReached)
		}
	}
}
