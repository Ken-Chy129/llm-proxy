package handler

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
	"github.com/gin-gonic/gin"
)

func TestChatCompletionsRejectsAnyGenStreamingBeforeStartingSSE(t *testing.T) {
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")
	anygenExec := executor.NewAnyGenExecutor(config.AnyGenConfig{
		APIKeyEnv: "TEST_ANYGEN_LLM_KEY",
		Models:    []string{"gpt-5.6-luna"},
	})
	r := router.New()
	r.Register(anygenExec, "anygen")
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

func TestSyncModelsRegistersAnyGenModelsLive(t *testing.T) {
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
	if _, err := r.Resolve("gpt-5.6-luna"); err != nil {
		t.Fatalf("synced AnyGen model was not registered: %v", err)
	}
	if got := r.BackendName("gpt-5.6-luna"); got != "anygen" {
		t.Fatalf("backend = %q, want anygen", got)
	}
}
