package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
	"github.com/gin-gonic/gin"
)

// syncedAnyGen returns an executor whose catalog holds exactly these models,
// discovered the way the real one is: from the upstream /models endpoint.
func syncedAnyGen(t *testing.T, models ...string) *executor.AnyGenExecutor {
	t.Helper()
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")

	payload := struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}{}
	for _, m := range models {
		payload.Data = append(payload.Data, struct {
			ID string `json:"id"`
		}{ID: m})
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal models payload: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
	}))
	t.Cleanup(server.Close)

	exec := executor.NewAnyGenExecutor(config.AnyGenConfig{
		BaseURL:   server.URL + "/api/v1",
		APIKeyEnv: "TEST_ANYGEN_LLM_KEY",
	})
	if _, err := exec.SyncModels(context.Background()); err != nil {
		t.Fatalf("SyncModels: %v", err)
	}
	return exec
}

func catalogEntries(t *testing.T, view gin.H) []gin.H {
	t.Helper()
	entries, ok := view["models"].([]gin.H)
	if !ok {
		t.Fatalf("models payload has unexpected type %T", view["models"])
	}
	return entries
}

// The provider card used to draw the routing table alone, so a model the
// upstream added stayed invisible until someone read release notes. It now
// reports the discovered catalog with each entry marked routed or not.
func TestCatalogViewSeparatesRoutedModelsFromWhatTheUpstreamOffers(t *testing.T) {
	anygenExec := syncedAnyGen(t, "gpt-5.5", "gpt-5.6-luna")

	cfg := &config.Config{AnyGen: config.AnyGenConfig{Enabled: true}}
	if err := cfg.SetRoutes([]config.ModelRoute{{
		Name:      "gpt-5.5",
		Providers: []config.ProviderRef{{Provider: "anygen"}},
	}}); err != nil {
		t.Fatalf("SetRoutes: %v", err)
	}
	r := router.New()
	r.SetRoutes([]router.Route{{Model: "gpt-5.5", Providers: []string{"anygen"}}})
	h := &AdminHandler{cfg: cfg, router: r, anygenExec: anygenExec}

	view := h.catalogView("anygen")
	if view == nil {
		t.Fatal("catalogView returned nothing for a provider with a catalog")
	}
	if view["total"] != 2 || view["unrouted"] != 1 {
		t.Fatalf("total=%v unrouted=%v, want 2 and 1", view["total"], view["unrouted"])
	}

	routed := map[string]bool{}
	for _, entry := range catalogEntries(t, view) {
		id, _ := entry["id"].(string)
		flag, _ := entry["routed"].(bool)
		routed[id] = flag
	}
	if !routed["gpt-5.5"] {
		t.Error("a published model was not marked routed")
	}
	if routed["gpt-5.6-luna"] {
		t.Error("a model no route names was marked routed")
	}
}

// A model renamed for one provider (Vertex wants dated ids) is served under its
// upstream id, so matching published names alone would report it as unrouted and
// invent work that is already done.
func TestCatalogViewCountsUpstreamRenamesAsRouted(t *testing.T) {
	anygenExec := syncedAnyGen(t, "claude-sonnet-4-5-20250929")

	cfg := &config.Config{AnyGen: config.AnyGenConfig{Enabled: true}}
	if err := cfg.SetRoutes([]config.ModelRoute{{
		Name:      "claude-sonnet-4-5",
		Providers: []config.ProviderRef{{Provider: "anygen", Upstream: "claude-sonnet-4-5-20250929"}},
	}}); err != nil {
		t.Fatalf("SetRoutes: %v", err)
	}
	r := router.New()
	r.SetRoutes([]router.Route{{Model: "claude-sonnet-4-5", Providers: []string{"anygen"}}})
	h := &AdminHandler{cfg: cfg, router: r, anygenExec: anygenExec}

	if view := h.catalogView("anygen"); view["unrouted"] != 0 {
		t.Fatalf("unrouted = %v, want 0 — the model is served under its upstream id", view["unrouted"])
	}
}

// Both API-key upstreams publish /v1/models, and the base URL is configured with
// or without the /v1 suffix — guessing wrong is a silent 404, so it is pinned.
func TestKimiExecutorDiscoversModelsFromEitherBaseURLShape(t *testing.T) {
	t.Setenv("TEST_RELAY_TOKEN", "sk-relay-test")
	for _, tc := range []struct{ name, suffix string }{
		{"base url without /v1", "/api"},
		{"base url with /v1", "/api/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"data":[{"id":"claude-fable-5"},{"id":"claude-opus-5"}]}`)
			}))
			defer server.Close()

			exec := executor.NewRelayExecutor(config.RelayConfig{
				BaseURL:      server.URL + tc.suffix,
				AuthTokenEnv: "TEST_RELAY_TOKEN",
			})
			models, err := exec.SyncModels(context.Background())
			if err != nil {
				t.Fatalf("SyncModels: %v", err)
			}
			if gotPath != "/api/v1/models" {
				t.Errorf("requested %q, want /api/v1/models", gotPath)
			}
			if len(models) != 2 || models[0] != "claude-fable-5" {
				t.Fatalf("models = %v", models)
			}
			// Discovery must not publish anything: routing decides what is served.
			if got := exec.Models(); len(got) != 0 {
				t.Fatalf("Models() = %v, want empty until routing assigns models", got)
			}
		})
	}
}

// publishRequest drives PublishModel the way the dashboard does.
func publishRequest(t *testing.T, h *AdminHandler, provider, model string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"provider":"` + provider + `","model":"` + model + `"}`
	c.Request = httptest.NewRequest(http.MethodPost, "/api/publish", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PublishModel(c)
	return w
}

// Publishing seeds the new route from its series default, so a discovered model
// inherits the same failover chain its siblings already use — that is the whole
// value over typing the name into the config page by hand.
func TestPublishModelSeedsTheChainFromTheSeriesDefault(t *testing.T) {
	anygenExec := syncedAnyGen(t, "claude-opus-4-6")

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		AnyGen: config.AnyGenConfig{Enabled: true},
		Series: config.SeriesConfig{"claude": []config.ProviderRef{
			{Provider: "claude_oauth"}, {Provider: "anygen"},
		}},
	}
	h := &AdminHandler{configPath: configPath, cfg: cfg, router: router.New(), anygenExec: anygenExec}

	if w := publishRequest(t, h, "anygen", "claude-opus-4-6"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	routes := cfg.Routes()
	if len(routes) != 1 || routes[0].Name != "claude-opus-4-6" {
		t.Fatalf("routes = %+v, want the published model", routes)
	}
	got := make([]string, len(routes[0].Providers))
	for i, ref := range routes[0].Providers {
		got[i] = ref.Provider
	}
	// claude_oauth has no catalog to contradict it, so the default order stands.
	if len(got) != 2 || got[0] != "claude_oauth" || got[1] != "anygen" {
		t.Fatalf("chain = %v, want the claude series default", got)
	}

	// The edit has to reach disk, or it is gone on the next restart.
	saved, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	if !strings.Contains(string(saved), "claude-opus-4-6") {
		t.Error("published model was not written to config.yaml")
	}
}

// A model discovered on one provider must end up reachable through it. Seeding
// purely from the series default would publish a gemini model onto a chain that
// never lists the provider that actually has it.
func TestPublishModelAlwaysIncludesTheDiscoveringProvider(t *testing.T) {
	anygenExec := syncedAnyGen(t, "gemini-3.5-flash")

	cfg := &config.Config{
		AnyGen: config.AnyGenConfig{Enabled: true},
		Series: config.SeriesConfig{"gemini": []config.ProviderRef{{Provider: "vertex"}}},
	}
	h := &AdminHandler{
		configPath: filepath.Join(t.TempDir(), "config.yaml"),
		cfg:        cfg,
		router:     router.New(),
		anygenExec: anygenExec,
	}

	if w := publishRequest(t, h, "anygen", "gemini-3.5-flash"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	chain := cfg.Routes()[0].Providers
	if !slices.ContainsFunc(chain, func(ref config.ProviderRef) bool { return ref.Provider == "anygen" }) {
		t.Fatalf("chain = %+v, want the discovering provider in it", chain)
	}
}

// A series default naming a provider whose catalog lacks the model would add a
// hop that always fails over, so it is left out.
func TestPublishModelSkipsDefaultsThatCannotServeTheModel(t *testing.T) {
	anygenExec := syncedAnyGen(t, "gpt-5.4-nano")

	t.Setenv("TEST_RELAY_TOKEN", "sk-relay-test")
	relayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"id":"claude-opus-5"}]}`)
	}))
	defer relayServer.Close()
	relayExec := executor.NewRelayExecutor(config.RelayConfig{
		BaseURL:      relayServer.URL + "/api",
		AuthTokenEnv: "TEST_RELAY_TOKEN",
	})
	if _, err := relayExec.SyncModels(context.Background()); err != nil {
		t.Fatalf("relay SyncModels: %v", err)
	}

	cfg := &config.Config{
		AnyGen: config.AnyGenConfig{Enabled: true},
		Relay:  config.RelayConfig{Enabled: true},
		Series: config.SeriesConfig{"gpt": []config.ProviderRef{
			{Provider: "relay"}, {Provider: "anygen"},
		}},
	}
	h := &AdminHandler{
		configPath: filepath.Join(t.TempDir(), "config.yaml"),
		cfg:        cfg,
		router:     router.New(),
		anygenExec: anygenExec,
		relayExec:  relayExec,
	}

	if w := publishRequest(t, h, "anygen", "gpt-5.4-nano"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	chain := cfg.Routes()[0].Providers
	if len(chain) != 1 || chain[0].Provider != "anygen" {
		t.Fatalf("chain = %+v, want only the provider that offers the model", chain)
	}
}

// Republishing a name would silently rewrite that model's chain — an edit
// nobody asked for.
func TestPublishModelRefusesAnAlreadyPublishedName(t *testing.T) {
	anygenExec := syncedAnyGen(t, "gpt-5.5")

	cfg := &config.Config{AnyGen: config.AnyGenConfig{Enabled: true}}
	if err := cfg.SetRoutes([]config.ModelRoute{{
		Name:      "gpt-5.5",
		Providers: []config.ProviderRef{{Provider: "codex"}},
	}}); err != nil {
		t.Fatalf("SetRoutes: %v", err)
	}
	h := &AdminHandler{
		configPath: filepath.Join(t.TempDir(), "config.yaml"),
		cfg:        cfg,
		router:     router.New(),
		anygenExec: anygenExec,
	}

	if w := publishRequest(t, h, "anygen", "gpt-5.5"); w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	chain := cfg.Routes()[0].Providers
	if len(chain) != 1 || chain[0].Provider != "codex" {
		t.Fatalf("chain = %+v, want the existing chain untouched", chain)
	}
}

// A published model becomes servable without a restart: publishing re-applies
// routing through the same path a config save uses.
func TestPublishModelMakesTheModelServableImmediately(t *testing.T) {
	anygenExec := syncedAnyGen(t, "gpt-5.4-nano")

	cfg := &config.Config{
		AnyGen: config.AnyGenConfig{Enabled: true},
		Series: config.SeriesConfig{"gpt": []config.ProviderRef{{Provider: "anygen"}}},
	}
	r := router.New()
	h := &AdminHandler{
		configPath: filepath.Join(t.TempDir(), "config.yaml"),
		cfg:        cfg,
		router:     r,
		anygenExec: anygenExec,
	}

	if _, err := r.Resolve("gpt-5.4-nano"); err == nil {
		t.Fatal("model was reachable before being published")
	}
	if w := publishRequest(t, h, "anygen", "gpt-5.4-nano"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if got := r.BackendName("gpt-5.4-nano"); got != "anygen" {
		t.Fatalf("backend = %q, want anygen right after publishing", got)
	}
}

// The card stops offering to publish what it just published.
func TestPublishedModelLeavesTheUnroutedList(t *testing.T) {
	anygenExec := syncedAnyGen(t, "gpt-5.4-nano")

	cfg := &config.Config{
		AnyGen: config.AnyGenConfig{Enabled: true},
		Series: config.SeriesConfig{"gpt": []config.ProviderRef{{Provider: "anygen"}}},
	}
	r := router.New()
	h := &AdminHandler{
		configPath: filepath.Join(t.TempDir(), "config.yaml"),
		cfg:        cfg,
		router:     r,
		anygenExec: anygenExec,
	}
	if view := h.catalogView("anygen"); view["unrouted"] != 1 {
		t.Fatalf("unrouted = %v before publishing, want 1", view["unrouted"])
	}
	publishRequest(t, h, "anygen", "gpt-5.4-nano")
	if view := h.catalogView("anygen"); view["unrouted"] != 0 {
		t.Fatalf("unrouted = %v after publishing, want 0", view["unrouted"])
	}
}

// Claude OAuth was long treated as undiscoverable, so its card reported only
// the routed models and a model the plan gained stayed invisible. The plan's
// catalog reaches the card the same way every other provider's does.
func TestClaudeOAuthCatalogReachesTheProviderCard(t *testing.T) {
	claudeExec := executor.NewClaudeOAuthExecutor(nil, nil)
	claudeExec.SetCatalog([]string{
		"claude-opus-5",
		"claude-opus-4-7",
		"claude-sonnet-4-5-20250929",
	})

	cfg := &config.Config{ClaudeOAuth: config.ClaudeOAuthConfig{Enabled: true}}
	if err := cfg.SetRoutes([]config.ModelRoute{
		{Name: "claude-opus-5", Providers: []config.ProviderRef{{Provider: "claude_oauth"}}},
		// Published under a friendly name, served under the dated upstream id —
		// that rename must not make the model look unrouted.
		{Name: "claude-sonnet-4-5", Providers: []config.ProviderRef{
			{Provider: "claude_oauth", Upstream: "claude-sonnet-4-5-20250929"},
		}},
	}); err != nil {
		t.Fatalf("SetRoutes: %v", err)
	}
	r := router.New()
	r.SetRoutes([]router.Route{
		{Model: "claude-opus-5", Providers: []string{"claude_oauth"}},
		{Model: "claude-sonnet-4-5", Providers: []string{"claude_oauth"}},
	})
	h := &AdminHandler{cfg: cfg, router: r, claudeExec: claudeExec}

	view := h.catalogView("claude_oauth")
	if view == nil {
		t.Fatal("claude_oauth reported no catalog")
	}
	if view["total"] != 3 || view["unrouted"] != 1 {
		t.Fatalf("total=%v unrouted=%v, want 3 and 1", view["total"], view["unrouted"])
	}
	routed := map[string]bool{}
	for _, entry := range catalogEntries(t, view) {
		id, _ := entry["id"].(string)
		flag, _ := entry["routed"].(bool)
		routed[id] = flag
	}
	if !routed["claude-sonnet-4-5-20250929"] {
		t.Error("a model served under an upstream rename was reported as unrouted")
	}
	if routed["claude-opus-4-7"] {
		t.Error("a model no route names was marked routed")
	}
}
