package router

import (
	"reflect"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
)

func TestMagpieRoutingAndFallback(t *testing.T) {
	t.Setenv("TEST_MAGPIE_KEY", "test-key")
	t.Setenv("TEST_RELAY_TOKEN", "test-relay")
	cfg := loadConfig(t, `
magpie:
  enabled: true
relay:
  enabled: true
models:
  - name: claude-opus-5-5
    providers:
      - magpie: claude/claude-opus-5-5
      - relay
`)
	m := executor.NewMagpieExecutor(config.MagpieConfig{APIKeyEnv: "TEST_MAGPIE_KEY"})
	p := &Providers{Magpie: m, Relay: executor.NewRelayExecutor(config.RelayConfig{AuthTokenEnv: "TEST_RELAY_TOKEN"})}
	r := New()
	Apply(r, cfg, p)
	e, err := r.Resolve("claude-opus-5-5")
	if err != nil || !reflect.DeepEqual(chainProviders(t, e), []string{"magpie", "relay"}) {
		t.Fatalf("chain %v, error %v", e, err)
	}
	if _, ok := executor.AsResponsesExecutor(e); !ok {
		t.Fatal("native Responses support lost")
	}
	t.Setenv("TEST_MAGPIE_KEY", "")
	e, err = r.Resolve("claude-opus-5-5")
	if err != nil || !reflect.DeepEqual(chainProviders(t, e), []string{"relay"}) {
		t.Fatal("missing Magpie key should keep the relay working")
	}
	t.Setenv("TEST_MAGPIE_KEY", "test-key")
	cfg.Magpie.Enabled = false
	Apply(r, cfg, p)
	if r.BackendName("claude-opus-5-5") != "relay" {
		t.Fatal("disabled Magpie still registered")
	}
}
