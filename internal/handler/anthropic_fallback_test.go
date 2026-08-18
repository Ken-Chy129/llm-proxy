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
	"github.com/Ken-Chy129/llm-proxy/internal/stats"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
	"github.com/gin-gonic/gin"
)

type fakeBackend struct {
	status int
	body   string
}

func (f *fakeBackend) Models() []string { return []string{"claude-opus-5"} }

func (f *fakeBackend) Execute(ctx context.Context, req *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	return &types.ChatCompletionResponse{}, nil
}

func (f *fakeBackend) ExecuteStream(ctx context.Context, req *types.ChatCompletionRequest, w io.Writer) (*types.Usage, error) {
	return &types.Usage{}, nil
}

func (f *fakeBackend) ExecuteAnthropicRaw(ctx context.Context, body []byte, h http.Header) ([]byte, int, error) {
	return []byte(f.body), f.status, nil
}

func (f *fakeBackend) OpenAnthropicStream(ctx context.Context, body []byte, h http.Header) (io.ReadCloser, int, error) {
	return io.NopCloser(strings.NewReader(f.body)), f.status, nil
}

// Relay overflow must be logged against the relay, otherwise the dashboard shows
// metered traffic as if the OAuth subscription had absorbed it.
func TestAnthropicLogsRelayWhenOAuthOverflows(t *testing.T) {
	db, err := stats.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open stats: %v", err)
	}
	defer db.Close()

	oauth := &fakeBackend{status: http.StatusTooManyRequests, body: "{}"}
	relay := &fakeBackend{status: http.StatusOK, body: "{\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}"}
	chain := executor.NewFallbackExecutor(oauth, relay, []string{"claude-opus-5"})

	r := router.New()
	r.RegisterModel("claude-opus-5", chain, "claude")
	h := NewAnthropicHandler(r, db)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader("{\"model\":\"claude-opus-5\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Messages(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (relay served it), body=%s", w.Code, w.Body.String())
	}

	logs, _, err := db.QueryLogs(10, 0, false, "")
	if err != nil {
		t.Fatalf("query logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("logged %d entries, want 1", len(logs))
	}
	if logs[0].Backend != "relay" {
		t.Fatalf("logged backend = %q, want relay", logs[0].Backend)
	}
	if !strings.Contains(logs[0].FailoverFrom, "claude") {
		t.Fatalf("failover_from = %q, want it to mention claude", logs[0].FailoverFrom)
	}
}
