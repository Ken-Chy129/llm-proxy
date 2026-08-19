package main

import (
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
)

func TestRegisterRelayKeepsAnthropicMessagesAvailableWhenAnyGenOverlaps(t *testing.T) {
	t.Setenv("TEST_RELAY_TOKEN", "relay-test-token")
	t.Setenv("TEST_ANYGEN_TOKEN", "anygen-test-token")

	r := router.New()
	r.SetBackendPriority([]string{"anygen", "relay"})
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
	if !registerRelay(r, relayExec, nil, true) {
		t.Fatal("registerRelay() = false, want true")
	}

	// Chat routing follows priority and chooses AnyGen.
	if resolved, err := r.Resolve("claude-fable-5"); err != nil || resolved != anygenExec {
		t.Fatalf("generic Resolve() = %T, %v; want AnyGen", resolved, err)
	}

	// Native Anthropic routing skips AnyGen and reaches the relay.
	resolved, backend, err := r.ResolveAnthropic("claude-fable-5")
	if err != nil {
		t.Fatalf("ResolveAnthropic() error: %v", err)
	}
	if resolved != relayExec {
		t.Fatalf("overlapping model resolved to %T, want relay executor", resolved)
	}
	if backend != "relay" {
		t.Fatalf("Anthropic backend = %q, want relay", backend)
	}
}

// A model both OAuth and relay can serve must resolve to a fallback chain, not
// to whichever backend registered last: OAuth is the cheap subscription and the
// relay is metered overflow, so OAuth has to be drained first.
func TestRegisterRelayWrapsOAuthModelsInFallbackChain(t *testing.T) {
	t.Setenv("TEST_RELAY_TOKEN", "relay-test-token")

	r := router.New()
	claudeExec := executor.NewClaudeOAuthExecutor(nil, []string{"claude-opus-5", "claude-sonnet-5"})
	r.Register(claudeExec, "claude")

	relayExec := executor.NewRelayExecutor(config.RelayConfig{
		Enabled:      true,
		AuthTokenEnv: "TEST_RELAY_TOKEN",
		Models: []config.ModelConfig{
			{Name: "claude-opus-5"},
			{Name: "claude-haiku-4-5-20251001"},
		},
	})
	if !registerRelay(r, relayExec, claudeExec, true) {
		t.Fatal("registerRelay() = false, want true")
	}

	// Shared model: fallback chain, reported as the OAuth backend because that
	// is what serves it until the subscription runs dry.
	shared, err := r.Resolve("claude-opus-5")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if _, ok := shared.(*executor.FallbackExecutor); !ok {
		t.Fatalf("shared model resolved to %T, want *executor.FallbackExecutor", shared)
	}
	if got := r.BackendName("claude-opus-5"); got != "claude" {
		t.Fatalf("shared model backend = %q, want claude", got)
	}

	// OAuth-only model is untouched.
	if got := r.BackendName("claude-sonnet-5"); got != "claude" {
		t.Fatalf("oauth-only backend = %q, want claude", got)
	}

	// Relay-only model stays on relay.
	relayOnly, err := r.Resolve("claude-haiku-4-5-20251001")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if relayOnly != relayExec {
		t.Fatalf("relay-only model resolved to %T, want relay executor", relayOnly)
	}
}
