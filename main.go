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
			// Seed plan_type from JWT for each account (quota % comes from API response headers)
			for _, t := range tokenStore.AllForProvider("codex") {
				pt := auth.ParseJWTPlanType(t.AccessToken)
				if pt == "" {
					pt = "unknown"
				}
				auth.QuotaCache.Set("codex:"+t.ID, &auth.QuotaInfo{
					AccountID: t.ID,
					PlanType:  pt,
					Primary:   &auth.RateWindow{Label: "5 小时限额", UsedPercent: 0},
					Secondary: &auth.RateWindow{Label: "周限额", UsedPercent: 0},
				})
			}
			log.Printf("seeded %d codex account quotas from JWT", len(tokenStore.AllForProvider("codex")))
		}
	}

	if err := server.Run(cfg, r, tokenStore, statsDB, claudeOAuth, codexOAuth, claudeExec, codexExec); err != nil {
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
