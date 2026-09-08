package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
	"github.com/gin-gonic/gin"
)

// streamRecorder adds the CloseNotifier that gin's c.Stream insists on, which
// httptest.ResponseRecorder does not provide.
type streamRecorder struct{ *httptest.ResponseRecorder }

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (streamRecorder) CloseNotify() <-chan bool { return make(chan bool) }

// rateLimitedStreamer stands in for a streaming provider (relay) that is out of
// quota: every call answers 429 before writing anything.
type rateLimitedStreamer struct{ calls int }

func (s *rateLimitedStreamer) Models() []string { return []string{"claude-sonnet-4-5"} }
func (s *rateLimitedStreamer) Execute(context.Context, *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	s.calls++
	return nil, &executor.HTTPError{Backend: "relay", Status: http.StatusTooManyRequests, Body: "rate limited"}
}
func (s *rateLimitedStreamer) ExecuteStream(context.Context, *types.ChatCompletionRequest, io.Writer) (*types.Usage, error) {
	s.calls++
	return nil, &executor.HTTPError{Backend: "relay", Status: http.StatusTooManyRequests, Body: "rate limited"}
}

// The production incident: relay -> anygen for claude-sonnet-4-5, relay gets
// rate limited, and the client saw
//
//	502 anygen error 400: Chat Completions supports non-streaming requests only
//
// because the streaming decision was made once for the chain's head and then
// applied to a provider that cannot stream. The chain now decides per provider,
// so the failover lands on AnyGen as a non-streaming call and the client gets a
// complete Responses event stream.
func TestResponsesStreamFailsOverFromStreamingRelayToNonStreamingAnyGen(t *testing.T) {
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")
	upstreamCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		var req types.ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		if req.Stream {
			t.Error("AnyGen upstream request must be non-streaming")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-anygen","object":"chat.completion","created":1,"model":"claude-sonnet-4-5","choices":[{"index":0,"message":{"role":"assistant","content":"served by anygen"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	}))
	defer server.Close()

	relay := &rateLimitedStreamer{}
	anygen := executor.NewAnyGenExecutor(config.AnyGenConfig{
		Enabled:   true,
		BaseURL:   server.URL + "/api/v1",
		APIKeyEnv: "TEST_ANYGEN_LLM_KEY",
	})
	anygen.SetServed([]string{"claude-sonnet-4-5"})

	r := router.New()
	r.SetProvider("relay", relay)
	r.SetProvider("anygen", anygen)
	r.SetRoutes([]router.Route{{Model: "claude-sonnet-4-5", Providers: []string{"relay", "anygen"}}})
	h := NewResponsesHandler(r, nil)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"stream":true,
		"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]
	}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.HandleResponses(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if relay.calls != 1 {
		t.Fatalf("relay calls = %d, want 1", relay.calls)
	}
	if upstreamCalls != 1 {
		t.Fatalf("anygen upstream calls = %d, want 1", upstreamCalls)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	body := w.Body.String()
	if strings.Contains(body, "non-streaming requests only") {
		t.Fatalf("the non-streaming provider was asked to stream:\n%s", body)
	}
	for _, want := range []string{
		"event: response.created",
		`"delta":"served by anygen"`,
		"event: response.output_text.done",
		"event: response.completed",
		`"input_tokens":5`,
		`"output_tokens":2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE response missing %q:\n%s", want, body)
		}
	}
	assertConsistentResponseID(t, body)
}

// The same chain over /v1/chat/completions used to hit the identical defect;
// a streaming client must get chunks from AnyGen after relay is exhausted.
func TestChatCompletionsStreamFailsOverFromStreamingRelayToNonStreamingAnyGen(t *testing.T) {
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req types.ChatCompletionRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Stream {
			t.Error("AnyGen upstream request must be non-streaming")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-anygen","object":"chat.completion","created":1,"model":"claude-sonnet-4-5","choices":[{"index":0,"message":{"role":"assistant","content":"served by anygen"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	}))
	defer server.Close()

	relay := &rateLimitedStreamer{}
	anygen := executor.NewAnyGenExecutor(config.AnyGenConfig{
		Enabled:   true,
		BaseURL:   server.URL + "/api/v1",
		APIKeyEnv: "TEST_ANYGEN_LLM_KEY",
	})
	anygen.SetServed([]string{"claude-sonnet-4-5"})

	r := router.New()
	r.SetProvider("relay", relay)
	r.SetProvider("anygen", anygen)
	r.SetRoutes([]router.Route{{Model: "claude-sonnet-4-5", Providers: []string{"relay", "anygen"}}})
	h := NewChatHandler(r, nil)

	gin.SetMode(gin.TestMode)
	w := newStreamRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"stream":true,
		"messages":[{"role":"user","content":"hello"}]
	}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.ChatCompletions(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, `"error"`) {
		t.Fatalf("stream carried an error:\n%s", body)
	}
	for _, want := range []string{`"object":"chat.completion.chunk"`, `"content":"served by anygen"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %q:\n%s", want, body)
		}
	}
}
