package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/pricing"
	"github.com/Ken-Chy129/llm-proxy/internal/stats"
	"github.com/gin-gonic/gin"
)

// newConfigHandler builds the slice of AdminHandler that UpdateConfig touches
// when no executor is wired: validation, cfg mutation, persistence, and pricing.
func newConfigHandler(t *testing.T) (*AdminHandler, *config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  port: 9090\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Server.AdminUser = "admin"
	cfg.Server.AdminPassword = "before"
	cfg.ClaudeOAuth.Models = []string{"claude-opus-5"}
	cfg.Codex.Models = []string{"gpt-5.5"}
	cfg.Vertex.Models = []config.ModelConfig{{Name: "sonnet", Model: "claude-sonnet-4-6"}}
	cfg.Kimi.Models = []config.ModelConfig{{Name: "kimi-k3", Model: "k3"}}
	cfg.Relay.Models = []config.ModelConfig{{Name: "relay-sonnet", Model: "claude-sonnet-4-5-20250929"}}
	cfg.AnyGen.Models = []string{"gpt-5.6-luna"}

	db, err := stats.Open(dir)
	if err != nil {
		t.Fatalf("open stats db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetPricing(pricing.New(nil))

	return &AdminHandler{configPath: path, cfg: cfg, statsDB: db}, cfg, path
}

func putConfig(t *testing.T, h *AdminHandler, body string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UpdateConfig(c)
	return w.Code, w.Body.String()
}

// The Models and Admin panels save independently. Each PUT therefore carries
// only its own section, and an absent section has to mean "leave it alone" —
// with value structs it decoded as an empty list and saving the admin password
// wiped every model list on the way through.
func TestUpdateConfigLeavesAbsentSectionsAlone(t *testing.T) {
	h, cfg, path := newConfigHandler(t)

	code, body := putConfig(t, h, `{"server":{"admin_user":"admin","admin_password":"after"}}`)
	if code != http.StatusOK {
		t.Fatalf("admin-only save failed (%d): %s", code, body)
	}

	if _, pass := cfg.AdminCreds(); pass != "after" {
		t.Errorf("password = %q, want %q", pass, "after")
	}
	if len(cfg.ClaudeOAuth.Models) != 1 || len(cfg.Codex.Models) != 1 ||
		len(cfg.Vertex.Models) != 1 || len(cfg.Kimi.Models) != 1 || len(cfg.Relay.Models) != 1 || len(cfg.AnyGen.Models) != 1 {
		t.Fatalf("an admin-only save clobbered the model lists: claude=%v codex=%v vertex=%v kimi=%v relay=%v anygen=%v",
			cfg.ClaudeOAuth.Models, cfg.Codex.Models, cfg.Vertex.Models, cfg.Kimi.Models, cfg.Relay.Models, cfg.AnyGen.Models)
	}

	// And the same must hold on disk, not just in memory.
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.Vertex.Models) != 1 || reloaded.Vertex.Models[0].Name != "sonnet" {
		t.Errorf("persisted vertex models = %v, want the original single entry", reloaded.Vertex.Models)
	}
	if len(reloaded.AnyGen.Models) != 1 || reloaded.AnyGen.Models[0] != "gpt-5.6-luna" {
		t.Errorf("persisted anygen models = %v, want the original fallback", reloaded.AnyGen.Models)
	}
}

func TestUpdateConfigPersistsRelayModels(t *testing.T) {
	h, cfg, path := newConfigHandler(t)

	code, body := putConfig(t, h, `{"relay":{"models":[
		{"name":"relay-opus","model":"claude-opus-4-5-20251101"},
		{"name":"claude-haiku-4-5-20251001","model":"claude-haiku-4-5-20251001"}
	]}}`)
	if code != http.StatusOK {
		t.Fatalf("save failed (%d): %s", code, body)
	}
	want := []config.ModelConfig{
		{Name: "relay-opus", Model: "claude-opus-4-5-20251101"},
		{Name: "claude-haiku-4-5-20251001"},
	}
	if !slices.Equal(cfg.Relay.Models, want) {
		t.Fatalf("relay models = %v, want %v", cfg.Relay.Models, want)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !slices.Equal(reloaded.Relay.Models, want) {
		t.Fatalf("persisted relay models = %v, want %v", reloaded.Relay.Models, want)
	}
}

// The mirror case: a models-only save must not disturb the admin credentials or
// revoke the tray token.
func TestUpdateConfigModelsOnlyKeepsServerSettings(t *testing.T) {
	h, cfg, _ := newConfigHandler(t)
	cfg.SetTrayToken("tray-keepme")

	code, body := putConfig(t, h, `{"claude_oauth":{"models":["claude-opus-5","claude-sonnet-5"]}}`)
	if code != http.StatusOK {
		t.Fatalf("models-only save failed (%d): %s", code, body)
	}
	if len(cfg.ClaudeOAuth.Models) != 2 {
		t.Errorf("claude models = %v, want 2 entries", cfg.ClaudeOAuth.Models)
	}
	if _, pass := cfg.AdminCreds(); pass != "before" {
		t.Errorf("password = %q, want it untouched", pass)
	}
	if cfg.TrayToken() != "tray-keepme" {
		t.Errorf("tray token = %q, want it untouched", cfg.TrayToken())
	}
}

// An upstream equal to the name is not a rename; storing it would print the name
// twice in config.yaml and show up in the editor as a mapping that can drift.
func TestUpdateConfigDropsIdentityMappings(t *testing.T) {
	h, cfg, _ := newConfigHandler(t)

	code, body := putConfig(t, h, `{"kimi":{"models":[
		{"name":"kimi-for-coding","model":"kimi-for-coding"},
		{"name":"kimi-k3","model":"k3"},
		{"name":"plain"}
	]}}`)
	if code != http.StatusOK {
		t.Fatalf("save failed (%d): %s", code, body)
	}
	want := []config.ModelConfig{
		{Name: "kimi-for-coding"},
		{Name: "kimi-k3", Model: "k3"},
		{Name: "plain"},
	}
	if len(cfg.Kimi.Models) != len(want) {
		t.Fatalf("kimi models = %v, want %v", cfg.Kimi.Models, want)
	}
	for i, w := range want {
		if cfg.Kimi.Models[i] != w {
			t.Errorf("kimi model %d = %+v, want %+v", i, cfg.Kimi.Models[i], w)
		}
	}
}

// A row with an upstream but no name publishes nothing and can serve nothing —
// silently dropping it would leave the admin believing it was saved.
func TestUpdateConfigRejectsUpstreamWithoutName(t *testing.T) {
	h, _, _ := newConfigHandler(t)
	code, body := putConfig(t, h, `{"vertex":{"models":[{"name":"","model":"claude-opus-5"}]}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body: %s", code, body)
	}
	var resp map[string]string
	json.Unmarshal([]byte(body), &resp)
	if !strings.Contains(resp["error"], "name") {
		t.Errorf("error = %q, want it to mention the missing name", resp["error"])
	}
}

// Saving a price makes it live immediately — the table is rebuilt, not left for
// the next restart.
func TestUpdateConfigAppliesPricingLive(t *testing.T) {
	h, _, _ := newConfigHandler(t)
	if _, ok := h.statsDB.Pricing().Lookup("mystery-model"); ok {
		t.Fatal("precondition: mystery-model should be unpriced")
	}

	code, body := putConfig(t, h, `{"pricing":{"models":[{"name":"mystery-model","input":2,"output":8}]}}`)
	if code != http.StatusOK {
		t.Fatalf("save failed (%d): %s", code, body)
	}
	p, ok := h.statsDB.Pricing().Lookup("mystery-model")
	if !ok {
		t.Fatal("price did not take effect without a restart")
	}
	if p.Input != 2 || p.Output != 8 {
		t.Errorf("price = %+v, want input 2 / output 8", p)
	}
}
