package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/auth"
	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
	"github.com/Ken-Chy129/llm-proxy/internal/stats"
	"github.com/gin-gonic/gin"
)

func TestMagpieAdminCatalogAndPause(t *testing.T) {
	t.Setenv("TEST_MAGPIE_KEY", "private-magpie-test")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"claude/claude-opus-5-5"}]}`)
	}))
	defer up.Close()
	cfg := &config.Config{Magpie: config.MagpieConfig{Enabled: true, BaseURL: up.URL, APIKeyEnv: "TEST_MAGPIE_KEY"}}
	if err := cfg.SetRoutes([]config.ModelRoute{{Name: "claude-opus-5-5", Providers: []config.ProviderRef{{Provider: "magpie", Upstream: "claude/claude-opus-5-5"}}}}); err != nil {
		t.Fatal(err)
	}
	e := executor.NewMagpieExecutor(cfg.Magpie)
	if _, err := e.SyncModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	db, err := stats.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := &AdminHandler{cfg: cfg, router: router.New(), tokenStore: auth.NewTokenStore(dir, ""), statsDB: db, magpieExec: e}
	h.applyRouting()
	if !h.providerAvailable("magpie") || h.catalogView("magpie")["unrouted"] != 0 {
		t.Fatal("Magpie not available, or alias not marked routed")
	}
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	h.Status(c)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"magpie"`) || strings.Contains(w.Body.String(), "private-magpie-test") {
		t.Fatalf("provider missing or secret exposed: status %d", w.Code)
	}
	if err := h.tokenStore.DisableBackend("magpie"); err != nil {
		t.Fatal(err)
	}
	if h.providerAvailable("magpie") {
		t.Fatal("paused provider still available")
	}
}

func TestMagpieNativeResponsesWinsOverChatAdapter(t *testing.T) {
	t.Setenv("TEST_MAGPIE_KEY", "test-magpie")
	var upstreamPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		var req map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&req)
		if !strings.Contains(string(req["tools"]), `"custom"`) || string(req["model"]) != `"claude/claude-opus-5-5"` {
			t.Error("native tools or upstream model lost")
		}
		io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"custom_tool_call\",\"call_id\":\"call_1\",\"name\":\"apply_patch\",\"input\":\"patch\"}],\"usage\":{\"input_tokens\":10,\"output_tokens\":3}}}\n\n")
	}))
	defer up.Close()
	m := executor.NewMagpieExecutor(config.MagpieConfig{BaseURL: up.URL, APIKeyEnv: "TEST_MAGPIE_KEY"})
	m.SetModels([]config.ModelConfig{{Name: "claude-opus-5-5", Model: "claude/claude-opus-5-5"}})
	r := router.New()
	r.SetProvider("magpie", m)
	// A non-native fallback must not force the preferred native provider through
	// the adapter, which rejects some Responses-only tool schemas.
	r.SetProvider("relay", &emptyNonStreamingExecutor{})
	r.SetRoutes([]router.Route{{Model: "claude-opus-5-5", Providers: []string{"magpie", "relay"}}})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"claude-opus-5-5","stream":true,"input":"hi","tools":[{"type":"custom","name":"apply_patch","format":{"type":"text"}}]}`))
	NewResponsesHandler(r, nil).HandleResponses(c)
	if w.Code != 200 || upstreamPath != "/v1/responses" || !strings.Contains(w.Body.String(), "custom_tool_call") {
		t.Fatalf("native path failed: status=%d path=%s body=%s", w.Code, upstreamPath, w.Body.String())
	}
}
