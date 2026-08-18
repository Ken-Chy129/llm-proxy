package main

import (
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
)

func TestRegisterRelayOverridesOverlappingAnyGenModel(t *testing.T) {
	t.Setenv("TEST_RELAY_TOKEN", "relay-test-token")
	t.Setenv("TEST_ANYGEN_TOKEN", "anygen-test-token")

	r := router.New()
	anygenExec := executor.NewAnyGenExecutor(config.AnyGenConfig{
		Enabled:   true,
		APIKeyEnv: "TEST_ANYGEN_TOKEN",
		Models:    []string{"claude-fable-5"},
	})
	r.Register(anygenExec, "anygen")

	relayExec := executor.NewRelayExecutor(config.RelayConfig{
		Enabled:      true,
		AuthTokenEnv: "TEST_RELAY_TOKEN",
		Models:       []config.ModelConfig{{Name: "claude-fable-5"}},
	})
	if !registerRelay(r, relayExec, true) {
		t.Fatal("registerRelay() = false, want true")
	}

	resolved, err := r.Resolve("claude-fable-5")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if resolved != relayExec {
		t.Fatalf("overlapping model resolved to %T, want relay executor", resolved)
	}
	if got := r.BackendName("claude-fable-5"); got != "relay" {
		t.Fatalf("backend = %q, want relay", got)
	}
	if _, ok := resolved.(executor.AnthropicExecutor); !ok {
		t.Fatal("resolved relay model does not support Anthropic Messages")
	}
}
