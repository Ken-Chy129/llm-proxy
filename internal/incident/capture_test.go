package incident

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
	"github.com/gin-gonic/gin"
)

func captureEngine(t *testing.T, dir string, handler gin.HandlerFunc) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CaptureFailures(config.FailureCaptureConfig{Enabled: true, Dir: dir, RetentionDays: 1}))
	r.POST("/v1/responses", handler)
	return r
}

func post(t *testing.T, r http.Handler, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret-api-key")
	req.Header.Set("x-api-key", "second-secret")
	r.ServeHTTP(httptest.NewRecorder(), req)
}

func captureEntries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestCapturesCompleteFailedRequestWithoutCredentials(t *testing.T) {
	dir := t.TempDir()
	r := captureEngine(t, dir, func(c *gin.Context) {
		c.Set("failure_capture_backend", "relay")
		c.Set("failure_capture_failover", []string{"claude_oauth"})
		MarkFailure(c, context.DeadlineExceeded)
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "upstream died"}})
	})
	body := `{"model":"claude-fable-5-1","input":"private prompt and tool output"}`
	post(t, r, body)

	entries := captureEntries(t, dir)
	if len(entries) != 1 {
		t.Fatalf("captures=%d, want 1", len(entries))
	}
	captureDir := filepath.Join(dir, entries[0].Name())
	request, _ := os.ReadFile(filepath.Join(captureDir, "request.json"))
	if string(request) != body {
		t.Fatalf("request=%s", request)
	}
	metadataBytes, _ := os.ReadFile(filepath.Join(captureDir, "metadata.json"))
	var metadata map[string]interface{}
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["backend"] != "relay" || metadata["model"] != "claude-fable-5-1" {
		t.Fatalf("metadata=%s", metadataBytes)
	}
	for _, name := range []string{"metadata.json", "request.json", "response.body", "replay.sh"} {
		content, _ := os.ReadFile(filepath.Join(captureDir, name))
		if strings.Contains(string(content), "secret-api-key") || strings.Contains(string(content), "second-secret") {
			t.Fatalf("credential leaked into %s", name)
		}
	}
	replay, _ := os.ReadFile(filepath.Join(captureDir, "replay.sh"))
	if strings.Contains(string(replay), "\n+") {
		t.Fatalf("replay script contains patch markers: %s", replay)
	}
	info, _ := os.Stat(filepath.Join(captureDir, "request.json"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("request mode=%o", info.Mode().Perm())
	}
}

func TestDoesNotCaptureRateLimits(t *testing.T) {
	dir := t.TempDir()
	r := captureEngine(t, dir, func(c *gin.Context) {
		MarkFailure(c, &testError{"usage_limit_reached: rate limit has been reached"})
		c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"type": "usage_limit_reached", "message": "rate limit has been reached"}})
	})
	post(t, r, `{"model":"m"}`)
	if entries := captureEntries(t, dir); len(entries) != 0 {
		t.Fatalf("rate limit captured: %v", entries)
	}
}

func TestCapturesCapacity429(t *testing.T) {
	dir := t.TempDir()
	r := captureEngine(t, dir, func(c *gin.Context) {
		MarkFailure(c, &testError{"Selected model is at capacity"})
		c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"type": "overloaded_error", "message": "Selected model is at capacity"}})
	})
	post(t, r, `{"model":"m","input":"session-specific context"}`)
	if entries := captureEntries(t, dir); len(entries) != 1 {
		t.Fatalf("capacity failure captures=%d, want 1", len(entries))
	}
}

func TestCapturesSuccessfulRequestWithNonRateLimitFailedAttempt(t *testing.T) {
	dir := t.TempDir()
	r := captureEngine(t, dir, func(c *gin.Context) {
		ctx := executor.WithAttemptRecorder(c.Request.Context())
		chain := executor.NewChain([]executor.Link{
			{Provider: "relay", Exec: &incidentStub{err: &executor.HTTPError{Backend: "relay", Status: 502, Body: "empty upstream stream"}}},
			{Provider: "claude_oauth", Exec: &incidentStub{}},
		})
		_, _ = chain.Execute(ctx, &types.ChatCompletionRequest{Model: "m"})
		c.Request = c.Request.WithContext(ctx)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	post(t, r, `{"model":"claude-fable-5-1"}`)
	if entries := captureEntries(t, dir); len(entries) != 1 {
		t.Fatalf("successful fallback captures=%d, want 1", len(entries))
	}
}

type incidentStub struct{ err error }

func (s *incidentStub) Models() []string { return []string{"m"} }
func (s *incidentStub) Execute(context.Context, *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	return &types.ChatCompletionResponse{}, s.err
}
func (s *incidentStub) ExecuteStream(context.Context, *types.ChatCompletionRequest, io.Writer) (*types.Usage, error) {
	return &types.Usage{}, s.err
}

func TestDoesNotCaptureClientCancellation(t *testing.T) {
	dir := t.TempDir()
	r := captureEngine(t, dir, func(c *gin.Context) {
		MarkFailure(c, context.Canceled)
		c.Status(http.StatusBadGateway)
	})
	post(t, r, `{"model":"m"}`)
	if entries := captureEntries(t, dir); len(entries) != 0 {
		t.Fatalf("cancellation captured: %v", entries)
	}
}

type testError struct{ message string }

func (e *testError) Error() string { return e.message }
