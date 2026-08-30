package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/pricing"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
	"github.com/Ken-Chy129/llm-proxy/internal/stats"
	"github.com/gin-gonic/gin"
)

// seedConfig is the routing table every test in this file starts from: one model
// per shape that matters (a plain chain, a chain with a rename, a single-provider
// model), plus the series defaults a model can inherit.
const seedConfig = `server:
  port: 9090
series:
  claude: [claude_oauth, relay]
  gpt: [codex, anygen]
  kimi: [kimi]
models:
  - name: claude-opus-5
  - name: claude-haiku-4-5
    providers:
      - vertex: claude-haiku-4-5-20251001
      - relay
  - name: kimi-k3
    providers:
      - kimi: k3
`

// newConfigHandler builds the slice of AdminHandler that UpdateConfig touches
// when no executor is wired: validation, cfg mutation, live re-routing, and
// persistence.
func newConfigHandler(t *testing.T) (*AdminHandler, *config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(seedConfig), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Server.AdminUser = "admin"
	cfg.Server.AdminPassword = "before"
	r := router.New()
	r.SetRoutes(router.RoutesFrom(cfg.Routes()))

	db, err := stats.Open(dir)
	if err != nil {
		t.Fatalf("open stats db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetPricing(pricing.New())

	return &AdminHandler{configPath: path, cfg: cfg, statsDB: db, router: r}, cfg, path
}

// chainOf returns a model's provider chain as plain names, which is what almost
// every assertion here is about.
func chainOf(t *testing.T, cfg *config.Config, model string) []string {
	t.Helper()
	for _, route := range cfg.Routes() {
		if route.Name != model {
			continue
		}
		out := make([]string, len(route.Providers))
		for i, ref := range route.Providers {
			out[i] = ref.Provider
		}
		return out
	}
	t.Fatalf("model %q is not in the routing table", model)
	return nil
}

func upstreamOf(t *testing.T, cfg *config.Config, model, provider string) string {
	t.Helper()
	for _, route := range cfg.Routes() {
		if route.Name != model {
			continue
		}
		for _, ref := range route.Providers {
			if ref.Provider == provider {
				return ref.Upstream
			}
		}
	}
	t.Fatalf("model %q has no %s link", model, provider)
	return ""
}

// liveChain reads a model's chain out of the running router, which is the state
// requests are actually served from — the point of the test is that a save
// reaches it, not just the config struct.
func liveChain(t *testing.T, h *AdminHandler, model string) []string {
	t.Helper()
	for _, route := range h.router.Routes() {
		if route.Model == model {
			return route.Providers
		}
	}
	t.Fatalf("model %q is not in the live routing table", model)
	return nil
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
// with value structs it decoded as an empty table and saving the admin password
// wiped the whole routing table on the way through.
func TestUpdateConfigLeavesAbsentSectionsAlone(t *testing.T) {
	h, cfg, path := newConfigHandler(t)

	code, body := putConfig(t, h, `{"server":{"admin_user":"admin","admin_password":"after"}}`)
	if code != http.StatusOK {
		t.Fatalf("admin-only save failed (%d): %s", code, body)
	}

	if _, pass := cfg.AdminCreds(); pass != "after" {
		t.Errorf("password = %q, want %q", pass, "after")
	}
	if got := len(cfg.Routes()); got != 3 {
		t.Fatalf("an admin-only save left %d models, want the original 3", got)
	}

	// And the same must hold on disk, not just in memory.
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := chainOf(t, reloaded, "claude-haiku-4-5"); len(got) != 2 || got[0] != "vertex" {
		t.Errorf("persisted haiku chain = %v, want the original vertex-first chain", got)
	}
	if got := upstreamOf(t, reloaded, "claude-haiku-4-5", "vertex"); got != "claude-haiku-4-5-20251001" {
		t.Errorf("persisted rename = %q, want the dated upstream id", got)
	}
}

// Reordering a chain is the core routing edit: it has to reach the live router
// immediately, not wait for a restart, and it has to survive one.
func TestUpdateConfigAppliesChainOrderLiveAndPersistsIt(t *testing.T) {
	h, cfg, path := newConfigHandler(t)
	if got := liveChain(t, h, "claude-haiku-4-5"); got[0] != "vertex" {
		t.Fatalf("precondition: live chain = %v, want vertex first", got)
	}

	code, body := putConfig(t, h, `{"models":[
		{"name":"claude-opus-5","providers":[{"provider":"claude_oauth"}]},
		{"name":"claude-haiku-4-5","providers":[
			{"provider":"relay","upstream":"claude-haiku-4-5-20251001"},
			{"provider":"vertex","upstream":"claude-haiku-4-5-20251001"}
		]},
		{"name":"kimi-k3","providers":[{"provider":"kimi","upstream":"k3"}]}
	]}`)
	if code != http.StatusOK {
		t.Fatalf("save failed (%d): %s", code, body)
	}
	if got := liveChain(t, h, "claude-haiku-4-5"); got[0] != "relay" || got[1] != "vertex" {
		t.Fatalf("live chain = %v, want [relay vertex] without a restart", got)
	}
	if got := chainOf(t, cfg, "claude-haiku-4-5"); got[0] != "relay" || got[1] != "vertex" {
		t.Fatalf("in-memory chain = %v, want [relay vertex]", got)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := chainOf(t, reloaded, "claude-haiku-4-5"); got[0] != "relay" || got[1] != "vertex" {
		t.Fatalf("persisted chain = %v, want [relay vertex]", got)
	}
	if got := upstreamOf(t, reloaded, "claude-haiku-4-5", "relay"); got != "claude-haiku-4-5-20251001" {
		t.Errorf("persisted relay upstream = %q, want the rename to survive the reorder", got)
	}
}

// A model saved without providers inherits its series default rather than being
// rejected — that is how the dashboard adds a model with one click.
func TestUpdateConfigFillsEmptyChainFromSeries(t *testing.T) {
	h, cfg, _ := newConfigHandler(t)

	code, body := putConfig(t, h, `{"models":[
		{"name":"claude-opus-5"},
		{"name":"claude-haiku-4-5","providers":[{"provider":"vertex","upstream":"claude-haiku-4-5-20251001"},{"provider":"relay"}]},
		{"name":"kimi-k3","providers":[{"provider":"kimi","upstream":"k3"}]},
		{"name":"gpt-5.5"}
	]}`)
	if code != http.StatusOK {
		t.Fatalf("save failed (%d): %s", code, body)
	}
	if got := chainOf(t, cfg, "gpt-5.5"); len(got) != 2 || got[0] != "codex" || got[1] != "anygen" {
		t.Fatalf("gpt-5.5 chain = %v, want the gpt series default [codex anygen]", got)
	}
}

// Editing the series defaults must not silently rewrite models that already
// resolved their chain — those are now explicit, and a series is only a template
// for the next model added.
func TestUpdateConfigSeriesEditLeavesResolvedModelsAlone(t *testing.T) {
	h, cfg, _ := newConfigHandler(t)

	code, body := putConfig(t, h, `{"series":{"claude":[{"provider":"vertex"},{"provider":"relay"}]}}`)
	if code != http.StatusOK {
		t.Fatalf("series save failed (%d): %s", code, body)
	}
	if got := chainOf(t, cfg, "claude-opus-5"); got[0] != "claude_oauth" {
		t.Fatalf("opus chain = %v, want it to keep the chain it already resolved to", got)
	}
	if got := cfg.SeriesDefaults()["claude"]; len(got) != 2 || got[0].Provider != "vertex" {
		t.Fatalf("claude series default = %v, want vertex first", got)
	}
}

// The mirror of the absent-section case: a models-only save must not disturb the
// admin credentials or revoke the tray token.
func TestUpdateConfigModelsOnlyKeepsServerSettings(t *testing.T) {
	h, cfg, _ := newConfigHandler(t)
	cfg.SetTrayToken("tray-keepme")

	code, body := putConfig(t, h, `{"models":[{"name":"claude-opus-5","providers":[{"provider":"relay"}]}]}`)
	if code != http.StatusOK {
		t.Fatalf("models-only save failed (%d): %s", code, body)
	}
	if got := len(cfg.Routes()); got != 1 {
		t.Errorf("routes = %d, want the single saved model", got)
	}
	if _, pass := cfg.AdminCreds(); pass != "before" {
		t.Errorf("password = %q, want it untouched", pass)
	}
	if cfg.TrayToken() != "tray-keepme" {
		t.Errorf("tray token = %q, want it untouched", cfg.TrayToken())
	}
}

// An upstream equal to the published name is not a rename; storing it would
// print the name twice in config.yaml and show in the editor as a mapping that
// can drift out of sync with the name above it.
func TestUpdateConfigDropsIdentityRenames(t *testing.T) {
	h, cfg, _ := newConfigHandler(t)

	code, body := putConfig(t, h, `{"models":[
		{"name":"claude-opus-5","providers":[{"provider":"relay","upstream":"claude-opus-5"}]},
		{"name":"kimi-k3","providers":[{"provider":"kimi","upstream":"k3"}]}
	]}`)
	if code != http.StatusOK {
		t.Fatalf("save failed (%d): %s", code, body)
	}
	if got := upstreamOf(t, cfg, "claude-opus-5", "relay"); got != "" {
		t.Errorf("upstream = %q, want it dropped as a non-rename", got)
	}
	if got := upstreamOf(t, cfg, "kimi-k3", "kimi"); got != "k3" {
		t.Errorf("upstream = %q, want the genuine rename kept", got)
	}
}

// A rejected save must leave both the config and the live router untouched:
// half-applying a bad table is how a dashboard typo takes models offline.
func TestUpdateConfigRejectsInvalidTablesWithoutMutation(t *testing.T) {
	for name, tc := range map[string]struct{ body, mentions string }{
		"unnamed model":      {`{"models":[{"name":"","providers":[{"provider":"relay"}]}]}`, "name"},
		"unknown provider":   {`{"models":[{"name":"claude-opus-5","providers":[{"provider":"mystery"}]}]}`, "mystery"},
		"duplicate provider": {`{"models":[{"name":"claude-opus-5","providers":[{"provider":"relay"},{"provider":"relay"}]}]}`, "relay"},
		"duplicate model":    {`{"models":[{"name":"claude-opus-5","providers":[{"provider":"relay"}]},{"name":"claude-opus-5","providers":[{"provider":"relay"}]}]}`, "claude-opus-5"},
		"unroutable series":  {`{"models":[{"name":"mystery-1"}]}`, "other"},
		"bad port":           {`{"server":{"port":70000}}`, "port"},
	} {
		t.Run(name, func(t *testing.T) {
			h, cfg, _ := newConfigHandler(t)
			before := len(cfg.Routes())

			code, response := putConfig(t, h, tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400; body=%s", code, response)
			}
			var resp map[string]string
			json.Unmarshal([]byte(response), &resp)
			if !strings.Contains(resp["error"], tc.mentions) {
				t.Errorf("error = %q, want it to mention %q", resp["error"], tc.mentions)
			}
			if got := len(cfg.Routes()); got != before {
				t.Fatalf("routing table mutated to %d models after a rejected request; want %d", got, before)
			}
			if got := len(h.router.Routes()); got != before {
				t.Fatalf("live router mutated to %d models after a rejected request; want %d", got, before)
			}
		})
	}
}
