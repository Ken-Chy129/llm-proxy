package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCodexResponsesCapacityFailureTriesNextAccount(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		tokens = append(tokens, token)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if token == "access-A" {
			io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"model_at_capacity\",\"message\":\"Selected model is at capacity. Please try a different model.\"}}}\n\n")
			return
		}
		io.WriteString(w, completedResponsesStream)
	}))
	defer server.Close()
	withCodexUpstream(t, server.URL, server.URL)
	exec, _ := newCodexTestExecutor(t, "B", "A")
	ctx := WithAttemptRecorder(context.Background())
	stream, err := exec.OpenResponsesStream(ctx, []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(stream)
	stream.Close()
	if !strings.Contains(string(body), "response.completed") {
		t.Fatalf("body=%s", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 2 || tokens[0] != "access-A" || tokens[1] != "access-B" {
		t.Fatalf("tokens=%v", tokens)
	}
	attempts := FailureAttempts(ctx)
	if len(attempts) != 1 || attempts[0].Status != http.StatusTooManyRequests || !strings.Contains(attempts[0].Error, "at capacity") {
		t.Fatalf("attempts=%+v", attempts)
	}
}

func TestCodexResponsesFailedEventIsAnError(t *testing.T) {
	_, err := preflightCodexResponsesStream(io.NopCloser(strings.NewReader("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"backend exploded\"}}}\n\n")))
	if err == nil || StatusFromError(err) != http.StatusBadGateway || !strings.Contains(err.Error(), "backend exploded") {
		t.Fatalf("error=%v status=%d", err, StatusFromError(err))
	}
}
