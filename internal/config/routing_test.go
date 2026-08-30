package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func loadYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func chainOf(t *testing.T, cfg *Config, model string) []ProviderRef {
	t.Helper()
	for _, route := range cfg.Models {
		if route.Name == model {
			return route.Providers
		}
	}
	t.Fatalf("model %q not in routing table", model)
	return nil
}

func providerNames(chain []ProviderRef) []string {
	out := make([]string, len(chain))
	for i, ref := range chain {
		out[i] = ref.Provider
	}
	return out
}

func TestModelInheritsItsSeriesChain(t *testing.T) {
	cfg, err := loadYAML(t, `
series:
  claude: [claude_oauth, relay]
models:
  - name: claude-opus-5
`)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	want := []string{"claude_oauth", "relay"}
	if got := providerNames(chainOf(t, cfg, "claude-opus-5")); !slices.Equal(got, want) {
		t.Fatalf("chain = %v, want %v", got, want)
	}
}

func TestModelChainOverridesItsSeries(t *testing.T) {
	cfg, err := loadYAML(t, `
series:
  claude: [claude_oauth, relay]
models:
  - name: claude-opus-5
    providers: [vertex]
`)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got := providerNames(chainOf(t, cfg, "claude-opus-5")); !slices.Equal(got, []string{"vertex"}) {
		t.Fatalf("chain = %v, want [vertex]", got)
	}
}

// A series default is copied into each model rather than shared, so editing one
// model's chain later cannot mutate its siblings.
func TestSeriesDefaultIsCopiedPerModel(t *testing.T) {
	cfg, err := loadYAML(t, `
series:
  claude: [claude_oauth, relay]
models:
  - name: claude-opus-5
  - name: claude-opus-4-6
`)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	first := chainOf(t, cfg, "claude-opus-5")
	first[0] = ProviderRef{Provider: "vertex"}
	if got := providerNames(chainOf(t, cfg, "claude-opus-4-6")); got[0] != "claude_oauth" {
		t.Fatalf("editing one model changed another: %v", got)
	}
}

func TestUpstreamRenameParsesInBothForms(t *testing.T) {
	cfg, err := loadYAML(t, `
models:
  - name: claude-haiku-4-5
    providers:
      - vertex: claude-haiku-4-5-20251001
      - provider: relay
        upstream: claude-haiku-4-5-20251001
`)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	chain := chainOf(t, cfg, "claude-haiku-4-5")
	for _, ref := range chain {
		if ref.Upstream != "claude-haiku-4-5-20251001" {
			t.Fatalf("%s upstream = %q, want the dated id", ref.Provider, ref.Upstream)
		}
	}
}

func TestRoutingRejectsInvalidTables(t *testing.T) {
	for name, body := range map[string]string{
		"unknown provider":   "models:\n  - name: x\n    providers: [mystery]\n",
		"duplicate provider": "models:\n  - name: x\n    providers: [relay, relay]\n",
		"duplicate model":    "models:\n  - name: x\n    providers: [relay]\n  - name: x\n    providers: [relay]\n",
		"no providers":       "models:\n  - name: mystery-1\n",
		"unnamed model":      "models:\n  - providers: [relay]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadYAML(t, body); err == nil {
				t.Fatal("Load() succeeded, want a validation error")
			}
		})
	}
}

// A model whose series has no default and that names no providers is a config
// mistake worth failing on: it would publish a name nothing can serve.
func TestModelWithoutAnyChainNamesItsSeries(t *testing.T) {
	_, err := loadYAML(t, "models:\n  - name: claude-opus-5\n")
	if err == nil {
		t.Fatal("Load() succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Fatalf("error %q does not name the series that lacks a default", err)
	}
}

func TestSeriesOfClassifiesByPrefix(t *testing.T) {
	for model, want := range map[string]string{
		"claude-opus-5":  "claude",
		"gpt-5.4":        "gpt",
		"gemini-3.1-pro": "gemini",
		"kimi-k3":        "kimi",
		"minimax-m3":     "other",
	} {
		if got := SeriesOf(model); got != want {
			t.Errorf("SeriesOf(%q) = %q, want %q", model, got, want)
		}
	}
}

// The saved file must reload identically, including the two rename forms, since
// every dashboard save rewrites the whole file.
func TestSaveRoundTripsTheRoutingTable(t *testing.T) {
	cfg, err := loadYAML(t, `
models:
  - name: claude-opus-5
    providers: [claude_oauth, relay]
  - name: claude-haiku-4-5
    providers:
      - vertex: claude-haiku-4-5-20251001
`)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	path := filepath.Join(t.TempDir(), "saved.yaml")
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := providerNames(chainOf(t, reloaded, "claude-opus-5")); !slices.Equal(got, []string{"claude_oauth", "relay"}) {
		t.Fatalf("chain after round-trip = %v", got)
	}
	if got := chainOf(t, reloaded, "claude-haiku-4-5")[0].Upstream; got != "claude-haiku-4-5-20251001" {
		t.Fatalf("upstream rename lost in round-trip: %q", got)
	}
}
