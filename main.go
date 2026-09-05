package main

import (
	"context"
	"flag"
	"log"
	"time"

	"github.com/Ken-Chy129/llm-proxy/internal/auth"
	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/pricing"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
	"github.com/Ken-Chy129/llm-proxy/internal/server"
	"github.com/Ken-Chy129/llm-proxy/internal/stats"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	r := router.New()
	tokenStore := auth.NewTokenStore(cfg.ClaudeOAuth.TokenDir, cfg.Server.AccountStrategy)
	r.SetChecker(tokenStore)
	auth.InitQuotaCache(tokenStore.Dir())
	// Discovered model lists are remembered across restarts so the dashboard can
	// say which models are *new* upstream rather than treating every one as new
	// on every boot.
	auth.InitModelCatalog(tokenStore.Dir())

	statsDB, err := stats.Open(cfg.ClaudeOAuth.TokenDir)
	if err != nil {
		log.Fatalf("open stats db: %v", err)
	}
	defer statsDB.Close()
	// Costing has to be installed before anything is served: it prices each
	// request as it is recorded, and backfills whatever history is unpriced.
	statsDB.SetPricing(pricing.New())

	// Every provider is constructed, whether or not it is enabled: the dashboard
	// can turn one on and add credentials at runtime, and a provider that exists
	// but is unregistered is simply skipped by every routing decision.
	vertexExec := executor.NewVertexExecutor(cfg.Vertex)
	if saved := auth.LoadGCPCredential(tokenStore.Dir()); saved != nil {
		if err := vertexExec.ApplyCredentials(context.Background(), saved.ProjectID, saved.Region, saved.Credentials, false); err != nil {
			log.Printf("apply saved gcp credentials: %v", err)
		} else {
			log.Printf("loaded uploaded gcp credentials (project=%s)", vertexExec.ProjectID())
		}
	}

	var claudeOAuth *auth.ClaudeOAuth
	var claudeExec *executor.ClaudeOAuthExecutor
	if cfg.ClaudeOAuth.Enabled {
		claudeOAuth = auth.NewClaudeOAuth(tokenStore)
		claudeOAuth.ServerPort = cfg.Server.Port
		claudeExec = executor.NewClaudeOAuthExecutor(claudeOAuth, nil)
		if len(tokenStore.AllForProvider("claude")) > 0 {
			log.Printf("fetching claude quotas for %d accounts...", len(tokenStore.AllForProvider("claude")))
			claudeOAuth.FetchAllQuotas(context.Background())
		}
	}

	var codexOAuth *auth.CodexOAuth
	var codexExec *executor.CodexExecutor
	if cfg.Codex.Enabled {
		codexOAuth = auth.NewCodexOAuth(tokenStore)
		codexOAuth.ServerPort = cfg.Server.Port
		codexExec = executor.NewCodexExecutor(codexOAuth, nil)
		if len(tokenStore.AllForProvider("codex")) > 0 {
			codexOAuth.RefreshAllTokens(context.Background())
			// The plan's model list is discoverable, so it is offered in the
			// dashboard when adding a model. What gets *served* still comes from
			// the routing table.
			if models, _, err := codexOAuth.FetchModels(context.Background()); err != nil {
				log.Printf("failed to fetch codex models: %v", err)
			} else {
				slugs := make([]string, len(models))
				for i, m := range models {
					slugs[i] = m.Slug
				}
				codexExec.SetCatalog(slugs)
				log.Printf("synced %d codex models", len(slugs))
			}
			log.Printf("fetching codex quotas for %d accounts...", len(tokenStore.AllForProvider("codex")))
			codexOAuth.FetchAllQuotas(context.Background())
		}
	}

	// Kimi API: OpenAI-compatible upstream, with the key read only from the
	// configured environment variable. Responses API requests from Codex are
	// translated by the handler; Anthropic Messages requests from Claude Code
	// are translated by KimiExecutor.
	kimiExec := executor.NewKimiExecutor(cfg.Kimi)

	// Anthropic-compatible relay: native Messages passthrough for Claude Code,
	// with Chat Completions/Responses translated by the shared API-key executor.
	relayExec := executor.NewRelayExecutor(cfg.Relay)

	// Both API-key upstreams publish /v1/models. Discovering their catalogs at
	// startup is what lets the provider cards show a model the upstream gained
	// but no route names yet; a failure here is never fatal, since serving the
	// routed models does not depend on it.
	for _, keyed := range []struct {
		name    string
		enabled bool
		exec    *executor.KimiExecutor
	}{
		{"kimi", cfg.Kimi.Enabled, kimiExec},
		{"relay", cfg.Relay.Enabled, relayExec},
	} {
		if !keyed.enabled || !keyed.exec.Configured() {
			continue
		}
		if models, err := keyed.exec.SyncModels(context.Background()); err != nil {
			log.Printf("failed to fetch %s models: %v", keyed.name, err)
		} else {
			log.Printf("synced %d %s models", len(models), keyed.name)
		}
	}

	// AnyGen API: app-scoped OpenAI-compatible Chat Completions + Models with
	// one sk-ag key. Its catalog is free to query, so it is refreshed at startup.
	anygenExec := executor.NewAnyGenExecutor(cfg.AnyGen)
	if cfg.AnyGen.Enabled && anygenExec.Configured() {
		if models, err := anygenExec.SyncModels(context.Background()); err != nil {
			log.Printf("failed to fetch anygen models: %v", err)
		} else {
			log.Printf("synced %d anygen models", len(models))
		}
		if credits, err := anygenExec.RefreshCredits(context.Background()); err != nil {
			log.Printf("failed to query anygen credits: %v", err)
		} else {
			log.Printf("verified anygen key (user=%s, credits=%s)", credits.UserID, credits.Credits)
		}
	}

	providers := &router.Providers{
		Claude: claudeExec,
		Codex:  codexExec,
		Vertex: vertexExec,
		Kimi:   kimiExec,
		Relay:  relayExec,
		AnyGen: anygenExec,
	}
	// One call installs both halves of routing: which providers exist, and which
	// models each may serve. Every later edit goes through the same function, so
	// the running state cannot drift from the config.
	router.Apply(r, cfg, providers)
	router.LogRoutes(r)

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

	// Periodically refresh OAuth quotas so account selection sees current
	// reset/limit data (window reset times roll forward over time).
	if claudeOAuth != nil || codexOAuth != nil || (cfg.AnyGen.Enabled && anygenExec.Configured()) {
		go func() {
			for range time.NewTicker(5 * time.Minute).C {
				if claudeOAuth != nil {
					claudeOAuth.FetchAllQuotas(context.Background())
				}
				if codexOAuth != nil {
					codexOAuth.FetchAllQuotas(context.Background())
				}
				if cfg.AnyGen.Enabled && anygenExec.Configured() {
					if _, err := anygenExec.RefreshCredits(context.Background()); err != nil {
						log.Printf("refresh anygen credits: %v", err)
					}
				}
			}
		}()
	}

	if err := server.Run(*configPath, cfg, r, tokenStore, keyStore, statsDB, claudeOAuth, codexOAuth, claudeExec, codexExec, vertexExec, kimiExec, relayExec, anygenExec); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
