package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
	"github.com/gin-gonic/gin"
)

// headerEchoResponses is a native Responses provider that records the client
// headers it received through the context and publishes an upstream
// turn-state, standing in for the Codex executor's header plumbing.
type headerEchoResponses struct {
	seen http.Header
}

func (s *headerEchoResponses) Models() []string { return []string{"gpt-5.5"} }
func (s *headerEchoResponses) Execute(context.Context, *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	return nil, nil
}
func (s *headerEchoResponses) ExecuteStream(context.Context, *types.ChatCompletionRequest, io.Writer) (*types.Usage, error) {
	return &types.Usage{}, nil
}
func (s *headerEchoResponses) OpenResponsesStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://upstream/responses", nil)
	executor.ApplyCodexClientHeadersForTest(ctx, req)
	s.seen = req.Header.Clone()
	upstream := http.Header{}
	upstream.Set("x-codex-turn-state", "turn-state-from-upstream")
	upstream.Set("Set-Cookie", "never")
	executor.RecordUpstreamHeadersForTest(ctx, upstream)
	return io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n")), nil
}

func TestResponsesRoundTripsCodexSessionHeaders(t *testing.T) {
	stub := &headerEchoResponses{}
	r := router.New()
	r.SetProvider("codex", stub)
	r.SetRoutes([]router.Route{{Model: "gpt-5.5", Providers: []string{"codex"}}})
	h := NewResponsesHandler(r, nil)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":[{"role":"user","content":"hi"}]}`))
	c.Request.Header.Set("x-codex-turn-state", "turn-state-from-client")
	c.Request.Header.Set("session_id", "sess-9")
	c.Request.Header.Set("Authorization", "Bearer proxy-key")
	h.HandleResponses(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if stub.seen.Get("x-codex-turn-state") != "turn-state-from-client" || stub.seen.Get("session_id") != "sess-9" {
		t.Fatalf("client session headers did not reach the executor: %v", stub.seen)
	}
	if stub.seen.Get("Authorization") != "" {
		t.Fatal("client Authorization must not be forwarded upstream")
	}
	if got := w.Header().Get("x-codex-turn-state"); got != "turn-state-from-upstream" {
		t.Fatalf("response x-codex-turn-state = %q", got)
	}
	if w.Header().Get("Set-Cookie") != "" {
		t.Fatal("non-allowlisted upstream header leaked to the client")
	}
}
