package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/router"
	"github.com/Ken-Chy129/llm-proxy/internal/stats"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
	"github.com/gin-gonic/gin"
)

// recordingBackend captures the model name it was asked to serve, which is the
// whole point of these tests: routing may read "model@provider", but no upstream
// may ever see it.
type recordingBackend struct {
	gotChatModel string
	gotRawModel  string
}

func (b *recordingBackend) Models() []string { return []string{"claude-sonnet-4-5"} }

func (b *recordingBackend) Execute(_ context.Context, req *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	b.gotChatModel = req.Model
	return &types.ChatCompletionResponse{
		Choices: []types.ChatCompletionChoice{{Message: &types.ChatResult{Role: "assistant", Content: "hi"}}},
	}, nil
}

func (b *recordingBackend) ExecuteStream(_ context.Context, req *types.ChatCompletionRequest, _ io.Writer) (*types.Usage, error) {
	b.gotChatModel = req.Model
	return &types.Usage{}, nil
}

func (b *recordingBackend) ExecuteAnthropicRaw(_ context.Context, body []byte, _ http.Header) ([]byte, int, error) {
	b.gotRawModel = modelOf(body)
	return []byte(`{"usage":{"input_tokens":3,"output_tokens":1}}`), http.StatusOK, nil
}

func (b *recordingBackend) OpenAnthropicStream(_ context.Context, body []byte, _ http.Header) (io.ReadCloser, int, error) {
	b.gotRawModel = modelOf(body)
	return io.NopCloser(strings.NewReader("")), http.StatusOK, nil
}

func modelOf(body []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &payload)
	return payload.Model
}

// The chat picker offers a model under every provider in its chain, and the
// non-preferred entries carry the router's "model@provider" override. The
// suffix selects the provider and must be consumed there: forwarding it is a
// 400 from the upstream, and it would also fork the stats rows and defeat the
// price lookup for one model.
func TestChatCompletionsRoutesProviderOverrideWithoutLeakingItUpstream(t *testing.T) {
	db, err := stats.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open stats: %v", err)
	}
	defer db.Close()

	head := &recordingBackend{}
	second := &recordingBackend{}
	r := router.New()
	r.SetProvider("relay", head)
	r.SetProvider("anygen", second)
	r.SetRoutes([]router.Route{{Model: "claude-sonnet-4-5", Providers: []string{"relay", "anygen"}}})

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"claude-sonnet-4-5@anygen","messages":[{"role":"user","content":"hi"}]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	NewChatHandler(r, db).ChatCompletions(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// The override picked the second provider, not the head of the chain.
	if head.gotChatModel != "" {
		t.Errorf("head of chain served %q, want the override to bypass it", head.gotChatModel)
	}
	if second.gotChatModel != "claude-sonnet-4-5" {
		t.Errorf("upstream model = %q, want the published name with no @provider suffix", second.gotChatModel)
	}

	logs, _, err := db.QueryLogs(10, 0, false, "")
	if err != nil {
		t.Fatalf("query logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("logged %d entries, want 1", len(logs))
	}
	// One model must stay one row in the stats, whichever provider served it.
	if logs[0].Model != "claude-sonnet-4-5" {
		t.Errorf("logged model = %q, want the published name", logs[0].Model)
	}
	if logs[0].Backend != "anygen" {
		t.Errorf("logged backend = %q, want anygen", logs[0].Backend)
	}
}

// The Messages endpoint forwards the caller's bytes, so stripping the override
// from the parsed name is not enough — the body carries it too.
func TestAnthropicMessagesStripsProviderOverrideFromForwardedBody(t *testing.T) {
	db, err := stats.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open stats: %v", err)
	}
	defer db.Close()

	head := &recordingBackend{}
	second := &recordingBackend{}
	r := router.New()
	r.SetProvider("claude_oauth", head)
	r.SetProvider("relay", second)
	r.SetRoutes([]router.Route{{Model: "claude-sonnet-4-5", Providers: []string{"claude_oauth", "relay"}}})

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-5@relay","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	NewAnthropicHandler(r, db).Messages(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if head.gotRawModel != "" {
		t.Errorf("head of chain served %q, want the override to bypass it", head.gotRawModel)
	}
	if second.gotRawModel != "claude-sonnet-4-5" {
		t.Errorf("forwarded body model = %q, want the published name with no @provider suffix", second.gotRawModel)
	}

	logs, _, err := db.QueryLogs(10, 0, false, "")
	if err != nil {
		t.Fatalf("query logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("logged %d entries, want 1", len(logs))
	}
	if logs[0].Model != "claude-sonnet-4-5" {
		t.Errorf("logged model = %q, want the published name", logs[0].Model)
	}
	if logs[0].Backend != "relay" {
		t.Errorf("logged backend = %q, want relay", logs[0].Backend)
	}
}

// probeBackend serves anything asked of it and records the model, standing in
// for a provider whose catalog lists a model the routing table has not
// published.
type probeBackend struct{ got string }

func (b *probeBackend) Models() []string { return nil }

func (b *probeBackend) Execute(_ context.Context, req *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	b.got = req.Model
	return &types.ChatCompletionResponse{
		Choices: []types.ChatCompletionChoice{{Message: &types.ChatResult{Role: "assistant", Content: "ok"}}},
	}, nil
}

func (b *probeBackend) ExecuteStream(_ context.Context, req *types.ChatCompletionRequest, _ io.Writer) (*types.Usage, error) {
	b.got = req.Model
	return &types.Usage{}, nil
}

func probeRequest(t *testing.T, h *ChatHandler, model string, session bool) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	if session {
		c.Set("dashboard_session", true)
	}
	h.ChatCompletions(c)
	return w
}

// An operator decides whether to publish a model by first finding out whether it
// is reachable at all. The chat tab can therefore aim an unpublished catalog
// model at a provider — but only from a logged-in dashboard session, only for
// ids that provider advertises, and without the routing table changing.
func TestChatCompletionsProbesUnpublishedCatalogModelForDashboardOnly(t *testing.T) {
	db, err := stats.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open stats: %v", err)
	}
	defer db.Close()

	backend := &probeBackend{}
	r := router.New()
	r.SetProvider("anygen", backend)
	// Deliberately empty: the model under test is NOT published anywhere.
	r.SetRoutes([]router.Route{})

	h := NewChatHandler(r, db)
	h.SetCatalogSource(func(provider string) []string {
		if provider == "anygen" {
			return []string{"minimax-m3"}
		}
		return nil
	})

	// A managed /v1 API key must not gain reach the routing table never granted.
	if w := probeRequest(t, h, "minimax-m3@anygen", false); w.Code != http.StatusNotFound {
		t.Errorf("api-key probe status = %d, want 404 (probing is dashboard-only)", w.Code)
	}
	if backend.got != "" {
		t.Fatalf("api-key probe reached the provider with %q, want no call at all", backend.got)
	}

	// The same request from the dashboard session is allowed to try it.
	if w := probeRequest(t, h, "minimax-m3@anygen", true); w.Code != http.StatusOK {
		t.Fatalf("dashboard probe status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if backend.got != "minimax-m3" {
		t.Errorf("probed model = %q, want the bare id with no @provider suffix", backend.got)
	}

	// A probe is a one-off request, not a publish: routing is untouched.
	if routes := r.Routes(); len(routes) != 0 {
		t.Errorf("probing changed the routing table to %+v, want it untouched", routes)
	}

	// An id the provider never advertised stays refused even for the dashboard,
	// so probing cannot be used to post arbitrary strings upstream.
	backend.got = ""
	if w := probeRequest(t, h, "not-in-any-catalog@anygen", true); w.Code != http.StatusNotFound {
		t.Errorf("uncatalogued probe status = %d, want 404", w.Code)
	}
	if backend.got != "" {
		t.Errorf("uncatalogued probe reached the provider with %q, want no call", backend.got)
	}
}
