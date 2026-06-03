package main

import (
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

	if cfg.Vertex.ProjectID != "" {
		vertexExec := executor.NewVertexExecutor(cfg.Vertex)
		r.Register(vertexExec, "vertex")
		log.Printf("registered vertex executor: %v", vertexExec.Models())
	}

	var claudeOAuth *auth.ClaudeOAuth
	if cfg.ClaudeOAuth.Enabled {
		claudeOAuth = auth.NewClaudeOAuth(tokenStore)
		models := cfg.ClaudeOAuth.Models
		if len(models) == 0 {
			models = []string{"claude-oauth"}
		}
		claudeExec := executor.NewClaudeOAuthExecutor(claudeOAuth, models)
		r.Register(claudeExec, "claude")
		log.Printf("registered claude oauth executor: %v", models)
	}

	var codexOAuth *auth.CodexOAuth
	if cfg.Codex.Enabled {
		codexOAuth = auth.NewCodexOAuth(tokenStore)
		models := cfg.Codex.Models
		if len(models) == 0 {
			models = []string{"gpt-4o"}
		}
		codexExec := executor.NewCodexExecutor(codexOAuth, models)
		r.Register(codexExec, "codex")
		log.Printf("registered codex executor: %v", models)
	}

	if err := server.Run(cfg, r, tokenStore, statsDB, claudeOAuth, codexOAuth); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
