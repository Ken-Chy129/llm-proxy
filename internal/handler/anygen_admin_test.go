package handler

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
	"github.com/gin-gonic/gin"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestChatCompletionsRejectsAnyGenStreamingBeforeStartingSSE(t *testing.T) {
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")
	anygenExec := executor.NewAnyGenExecutor(config.AnyGenConfig{
		APIKeyEnv: "TEST_ANYGEN_LLM_KEY",
	})
	anygenExec.SetServed([]string{"gpt-5.6-luna"})
	r := router.New()
	r.SetProvider("anygen", anygenExec)
	r.SetRoutes([]router.Route{{Model: "gpt-5.6-luna", Providers: []string{"anygen"}}})
	h := NewChatHandler(r, nil)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{
		"model":"gpt-5.6-luna",
		"stream":true,
		"messages":[{"role":"user","content":"hello"}]
	}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.ChatCompletions(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got == "text/event-stream" {
		t.Fatalf("unsupported stream started SSE response: %q", got)
	}
}

// A sync refreshes what AnyGen *offers*. Serving it is a routing decision, so a
// synced model that no route names must not become reachable on its own — that
// is what used to let a 30-model catalog claim names nobody published.
func TestSyncModelsUpdatesTheCatalogWithoutServingIt(t *testing.T) {
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			t.Fatalf("path = %q, want /api/v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[{"id":"gpt-5.6-luna","object":"model","owned_by":"anygen"}]}`)
	}))
	defer server.Close()

	cfg := &config.Config{AnyGen: config.AnyGenConfig{
		Enabled:   true,
		BaseURL:   server.URL + "/api/v1",
		APIKeyEnv: "TEST_ANYGEN_LLM_KEY",
	}}
	anygenExec := executor.NewAnyGenExecutor(cfg.AnyGen)
	r := router.New()
	h := &AdminHandler{cfg: cfg, router: r, anygenExec: anygenExec}

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/sync-models", nil)
	h.SyncModels(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if got := anygenExec.Catalog(); len(got) != 1 || got[0] != "gpt-5.6-luna" {
		t.Fatalf("catalog = %v, want the synced model", got)
	}
	if _, err := r.Resolve("gpt-5.6-luna"); err == nil {
		t.Fatal("a synced but unrouted model became reachable")
	}

	// Once a route names it, the same sync makes it servable.
	if err := cfg.SetRoutes([]config.ModelRoute{{
		Name:      "gpt-5.6-luna",
		Providers: []config.ProviderRef{{Provider: "anygen"}},
	}}); err != nil {
		t.Fatalf("SetRoutes: %v", err)
	}
	h.applyRouting()
	if got := r.BackendName("gpt-5.6-luna"); got != "anygen" {
		t.Fatalf("backend = %q, want anygen once routed", got)
	}
}

func TestAnyGenCreditsBecomeQuotaPayload(t *testing.T) {
	quota := anyGenCreditsQuota(executor.AnyGenCredits{
		Verified: true,
		UserID:   "7530827736760778771",
		Credits:  "51139",
	})

	if quota["kind"] != "credits" {
		t.Fatalf("kind = %v, want credits", quota["kind"])
	}
	if quota["account_id"] != "anygen" {
		t.Fatalf("account_id = %v, want anygen", quota["account_id"])
	}
	if quota["credits"] != "51139" {
		t.Fatalf("credits = %v, want 51139", quota["credits"])
	}
	if quota["has_real_data"] != true {
		t.Fatalf("has_real_data = %v, want true", quota["has_real_data"])
	}
}

func TestRefreshQuotaSupportsAnyGenCredits(t *testing.T) {
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"verified":true,"user_id":"7530827736760778771","credits":"52141"}`)
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	oldTransport := http.DefaultTransport
	http.DefaultTransport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme = serverURL.Scheme
		req.URL.Host = serverURL.Host
		return oldTransport.RoundTrip(req)
	})
	t.Cleanup(func() { http.DefaultTransport = oldTransport })

	anygenExec := executor.NewAnyGenExecutor(config.AnyGenConfig{APIKeyEnv: "TEST_ANYGEN_LLM_KEY"})
	h := &AdminHandler{anygenExec: anygenExec}

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "provider", Value: "anygen"}, {Key: "id", Value: "anygen"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/api/refresh-quota/anygen/anygen", nil)
	h.RefreshQuota(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"credits":"52141"`)) {
		t.Fatalf("response missing refreshed credits: %s", w.Body.String())
	}
}
