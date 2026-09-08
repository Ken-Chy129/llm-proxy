package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
	"github.com/Ken-Chy129/llm-proxy/internal/stats"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
	"github.com/gin-gonic/gin"
)

// chatOnlyBackend speaks Chat Completions only, like relay and claude_oauth do
// for a Responses request. streamErr, when set, is returned after the executor
// has already written its opening chunk — the shape of a real translator that
// discovers a broken upstream mid-translation.
type chatOnlyBackend struct {
	name      string
	streamErr error
	served    bool
}

func (b *chatOnlyBackend) Models() []string { return []string{"claude-opus-5"} }

func (b *chatOnlyBackend) Execute(_ context.Context, req *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	if b.streamErr != nil {
		return nil, b.streamErr
	}
	b.served = true
	return &types.ChatCompletionResponse{
		Choices: []types.ChatCompletionChoice{{Message: &types.ChatResult{Role: "assistant", Content: "hi from " + b.name}}},
	}, nil
}

func (b *chatOnlyBackend) ExecuteStream(_ context.Context, req *types.ChatCompletionRequest, w io.Writer) (*types.Usage, error) {
	// Written before the failure is known, exactly as the Anthropic translators
	// do: the opening role chunk goes out as soon as the upstream returns 200.
	fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
	if b.streamErr != nil {
		return &types.Usage{}, b.streamErr
	}
	b.served = true
	fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi from "+b.name+"\"}}]}\n\n")
	fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
	return &types.Usage{}, nil
}

func postResponses(t *testing.T, h *ResponsesHandler) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"claude-opus-5","stream":true,"input":[{"role":"user","content":"hi"}]}`))
	h.HandleResponses(c)
	return w
}

// The reported bug: a claude_oauth -> relay chain served through /v1/responses
// returned 500 when the relay's stream came back unusable, even though the
// whole point of a chain is that a failing provider hands off to the next one.
// Both providers here need the Chat Completions adapter, which used to opt the
// chain out of adapter-based failover entirely.
func TestResponsesFallsOverWhenEveryProviderNeedsTheAdapter(t *testing.T) {
	db, err := stats.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open stats: %v", err)
	}
	defer db.Close()

	oauth := &chatOnlyBackend{
		name:      "claude_oauth",
		streamErr: &executor.HTTPError{Backend: "claude oauth", Status: http.StatusTooManyRequests, Body: "quota exhausted"},
	}
	relay := &chatOnlyBackend{name: "relay"}

	r := router.New()
	r.SetProvider("claude_oauth", oauth)
	r.SetProvider("relay", relay)
	r.SetRoutes([]router.Route{{Model: "claude-opus-5", Providers: []string{"claude_oauth", "relay"}}})

	w := postResponses(t, NewResponsesHandler(r, db))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (relay served it), body=%s", w.Code, w.Body.String())
	}
	if !relay.served {
		t.Error("relay was never tried, so the chain did not fall over")
	}
	if !strings.Contains(w.Body.String(), "hi from relay") {
		t.Errorf("body = %q, want the relay's answer", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "response.completed") {
		t.Error("the translated stream must terminate with response.completed")
	}

	logs, _, _ := db.QueryLogs(10, 0, false, "")
	if len(logs) != 1 {
		t.Fatalf("logged %d rows, want 1", len(logs))
	}
	if logs[0].Backend != "relay" {
		t.Errorf("logged backend = %q, want relay (it is what served)", logs[0].Backend)
	}
	if logs[0].Status != http.StatusOK {
		t.Errorf("logged status = %d, want 200", logs[0].Status)
	}
	if !strings.Contains(logs[0].FailoverFrom, "claude_oauth") {
		t.Errorf("failover_from = %q, want it to name claude_oauth", logs[0].FailoverFrom)
	}
}

// Once the last provider fails there is nothing left to try, so the caller does
// get an error — but it must carry the upstream's own explanation and status
// rather than a blanket 500 about a missing finish reason.
func TestResponsesReportsTheUpstreamReasonWhenTheChainIsExhausted(t *testing.T) {
	db, err := stats.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open stats: %v", err)
	}
	defer db.Close()

	upstreamErr := &executor.HTTPError{Backend: "relay", Status: http.StatusBadGateway, Body: "upstream is overloaded"}
	oauth := &chatOnlyBackend{
		name:      "claude_oauth",
		streamErr: &executor.HTTPError{Backend: "claude oauth", Status: http.StatusTooManyRequests, Body: "quota exhausted"},
	}
	relay := &chatOnlyBackend{name: "relay", streamErr: upstreamErr}

	r := router.New()
	r.SetProvider("claude_oauth", oauth)
	r.SetProvider("relay", relay)
	r.SetRoutes([]router.Route{{Model: "claude-opus-5", Providers: []string{"claude_oauth", "relay"}}})

	w := postResponses(t, NewResponsesHandler(r, db))

	if w.Code == http.StatusOK {
		t.Fatal("status = 200, want an error once every provider has failed")
	}
	if !strings.Contains(w.Body.String(), "upstream is overloaded") {
		t.Errorf("body = %q, want the upstream's reason instead of a bare finish-reason complaint", w.Body.String())
	}

	logs, _, _ := db.QueryLogs(10, 0, false, "")
	if len(logs) != 1 {
		t.Fatalf("logged %d rows, want 1", len(logs))
	}
	if strings.Contains(logs[0].Error, "without a finish reason") {
		t.Errorf("logged error = %q, want the upstream's reason, not the translator's symptom", logs[0].Error)
	}
}
