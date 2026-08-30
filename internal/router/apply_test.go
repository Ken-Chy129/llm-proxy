package router

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/auth"
	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
)

func loadConfig(t *testing.T, body string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// signedInClaude builds a Claude executor with one account, which is what makes
// it usable: an executor with no accounts is deliberately skipped by routing.
func signedInClaude(t *testing.T) *executor.ClaudeOAuthExecutor {
	t.Helper()
	store := auth.NewTokenStore(t.TempDir(), "")
	if err := store.Add(&auth.TokenData{Provider: "claude", ID: "acct", Email: "a@example.com"}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return executor.NewClaudeOAuthExecutor(auth.NewClaudeOAuth(store), nil)
}

// Apply is the only path from configuration to live routing, so this covers the
// property the old hard-coded relay registration existed to provide: a model
// both a subscription and a paid relay can serve drains the subscription first.
func TestApplyBuildsChainsFromConfig(t *testing.T) {
	t.Setenv("TEST_RELAY_TOKEN", "relay-token")
	cfg := loadConfig(t, `
claude_oauth:
  enabled: true
relay:
  enabled: true
  auth_token_env: TEST_RELAY_TOKEN
models:
  - name: claude-opus-5
    providers: [claude_oauth, relay]
`)

	r := New()
	Apply(r, cfg, &Providers{
		Claude: signedInClaude(t),
		Relay:  executor.NewRelayExecutor(config.RelayConfig{AuthTokenEnv: "TEST_RELAY_TOKEN"}),
	})

	exec, err := r.Resolve("claude-opus-5")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	want := []string{"claude_oauth", "relay"}
	if got := chainProviders(t, exec); !reflect.DeepEqual(got, want) {
		t.Fatalf("chain = %v, want %v", got, want)
	}
}

// A provider missing its credentials is skipped rather than breaking the model,
// which is what lets a chain be configured before every provider is set up.
func TestApplySkipsUnconfiguredProviders(t *testing.T) {
	cfg := loadConfig(t, `
claude_oauth:
  enabled: true
relay:
  enabled: true
  auth_token_env: TEST_RELAY_TOKEN_UNSET
models:
  - name: claude-opus-5
    providers: [claude_oauth, relay]
`)

	r := New()
	Apply(r, cfg, &Providers{
		Claude: signedInClaude(t),
		Relay:  executor.NewRelayExecutor(config.RelayConfig{AuthTokenEnv: "TEST_RELAY_TOKEN_UNSET"}),
	})

	exec, err := r.Resolve("claude-opus-5")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if got := chainProviders(t, exec); !reflect.DeepEqual(got, []string{"claude_oauth"}) {
		t.Fatalf("chain = %v, want [claude_oauth] with relay unconfigured", got)
	}
}

// A disabled provider must not serve, even when models still name it.
func TestApplyDropsDisabledProviders(t *testing.T) {
	t.Setenv("TEST_RELAY_TOKEN", "relay-token")
	cfg := loadConfig(t, `
claude_oauth:
  enabled: false
relay:
  enabled: true
  auth_token_env: TEST_RELAY_TOKEN
models:
  - name: claude-opus-5
    providers: [claude_oauth, relay]
`)

	r := New()
	Apply(r, cfg, &Providers{
		Claude: signedInClaude(t),
		Relay:  executor.NewRelayExecutor(config.RelayConfig{AuthTokenEnv: "TEST_RELAY_TOKEN"}),
	})

	if got := r.BackendName("claude-opus-5"); got != "relay" {
		t.Fatalf("BackendName() = %q, want relay", got)
	}
}

// Each provider is told exactly which models route to it, with the upstream
// rename attached — that mapping is the only thing renames are still for.
func TestProviderModelsCarriesUpstreamRenames(t *testing.T) {
	cfg := loadConfig(t, `
models:
  - name: claude-haiku-4-5
    providers:
      - vertex: claude-haiku-4-5-20251001
  - name: kimi-k3
    providers:
      - kimi: k3
`)

	byProvider := ProviderModels(cfg.Routes())
	vertex := byProvider["vertex"]
	if len(vertex) != 1 || vertex[0].Name != "claude-haiku-4-5" || vertex[0].Model != "claude-haiku-4-5-20251001" {
		t.Fatalf("vertex models = %+v", vertex)
	}
	if got := byProvider["kimi"]; len(got) != 1 || got[0].Model != "k3" {
		t.Fatalf("kimi models = %+v", got)
	}
}
