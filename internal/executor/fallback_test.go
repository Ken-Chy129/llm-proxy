package executor

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

type stubAnthropic struct {
	name     string
	status   int
	body     string
	err      error
	rawCalls int
}

func (s *stubAnthropic) Models() []string { return []string{"m"} }

func (s *stubAnthropic) Execute(ctx context.Context, req *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &types.ChatCompletionResponse{Model: s.name}, nil
}

func (s *stubAnthropic) ExecuteStream(ctx context.Context, req *types.ChatCompletionRequest, w io.Writer) (*types.Usage, error) {
	if s.err != nil {
		return nil, s.err
	}
	io.WriteString(w, s.name)
	return &types.Usage{}, nil
}

func (s *stubAnthropic) ExecuteAnthropicRaw(ctx context.Context, body []byte, h http.Header) ([]byte, int, error) {
	s.rawCalls++
	if s.err != nil {
		return nil, 0, s.err
	}
	return []byte(s.body), s.status, nil
}

func (s *stubAnthropic) OpenAnthropicStream(ctx context.Context, body []byte, h http.Header) (io.ReadCloser, int, error) {
	if s.err != nil {
		return nil, 0, s.err
	}
	return io.NopCloser(strings.NewReader(s.body)), s.status, nil
}

func TestFallbackUsesPrimaryWhenPrimarySucceeds(t *testing.T) {
	primary := &stubAnthropic{name: "primary", status: 200, body: "{}"}
	secondary := &stubAnthropic{name: "secondary", status: 200, body: "{}"}
	fb := NewChain([]Link{{Provider: "claude_oauth", Exec: primary}, {Provider: "relay", Exec: secondary}})

	_, status, err := fb.ExecuteAnthropicRaw(context.Background(), []byte("{}"), nil)
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if secondary.rawCalls != 0 {
		t.Fatalf("secondary called %d times, want 0", secondary.rawCalls)
	}
}

func TestFallbackSwitchesToSecondaryOn429(t *testing.T) {
	primary := &stubAnthropic{name: "primary", status: 429, body: `{"error":"rate_limit"}`}
	secondary := &stubAnthropic{name: "secondary", status: 200, body: `{"ok":true}`}
	fb := NewChain([]Link{{Provider: "claude_oauth", Exec: primary}, {Provider: "relay", Exec: secondary}})

	body, status, err := fb.ExecuteAnthropicRaw(context.Background(), []byte("{}"), nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if status != 200 {
		t.Fatalf("status=%d, want 200 from secondary", status)
	}
	if !strings.Contains(string(body), "ok") {
		t.Fatalf("body=%s, want secondary response", body)
	}
	if secondary.rawCalls != 1 {
		t.Fatalf("secondary called %d times, want 1", secondary.rawCalls)
	}
}

func TestFallbackDoesNotSwitchOnClientError(t *testing.T) {
	primary := &stubAnthropic{name: "primary", status: 400, body: `{"error":"bad"}`}
	secondary := &stubAnthropic{name: "secondary", status: 200, body: "{}"}
	fb := NewChain([]Link{{Provider: "claude_oauth", Exec: primary}, {Provider: "relay", Exec: secondary}})

	_, status, err := fb.ExecuteAnthropicRaw(context.Background(), []byte("{}"), nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if status != 400 {
		t.Fatalf("status=%d, want 400 passed through", status)
	}
	if secondary.rawCalls != 0 {
		t.Fatalf("secondary called %d times, want 0 (400 is the client's fault)", secondary.rawCalls)
	}
}

func TestFallbackRecordsWhichBackendServed(t *testing.T) {
	primary := &stubAnthropic{name: "primary", status: 429, body: "{}"}
	secondary := &stubAnthropic{name: "secondary", status: 200, body: "{}"}
	fb := NewChain([]Link{{Provider: "claude_oauth", Exec: primary}, {Provider: "relay", Exec: secondary}})

	ctx, getBackend := WithBackendRecorder(context.Background())
	if _, _, err := fb.ExecuteAnthropicRaw(ctx, []byte("{}"), nil); err != nil {
		t.Fatalf("err=%v", err)
	}
	if got := getBackend(); got != "relay" {
		t.Fatalf("served backend = %q, want relay", got)
	}
}

// A chain is not limited to two providers: exhaustion walks it to the end.
func TestChainWalksEveryProviderInOrder(t *testing.T) {
	first := &stubAnthropic{name: "first", status: 429, body: "{}"}
	second := &stubAnthropic{name: "second", status: 500, body: "{}"}
	third := &stubAnthropic{name: "third", status: 200, body: `{"ok":true}`}
	chain := NewChain([]Link{
		{Provider: "claude_oauth", Exec: first},
		{Provider: "vertex", Exec: second},
		{Provider: "relay", Exec: third},
	})

	ctx, getBackend := WithBackendRecorder(context.Background())
	_, status, err := chain.ExecuteAnthropicRaw(ctx, []byte("{}"), nil)
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v, want the third provider to serve", status, err)
	}
	if got := getBackend(); got != "relay" {
		t.Errorf("served backend = %q, want relay", got)
	}
	if got := BackendFallbackFrom(ctx); !reflect.DeepEqual(got, []string{"claude_oauth", "vertex"}) {
		t.Errorf("failover trail = %v, want both exhausted providers in order", got)
	}
}

// The last provider's failure is what the client sees; a chain must not invent
// a success or swallow the status.
func TestChainReportsTheLastFailureWhenEveryProviderIsExhausted(t *testing.T) {
	chain := NewChain([]Link{
		{Provider: "claude_oauth", Exec: &stubAnthropic{status: 429, body: "{}"}},
		{Provider: "relay", Exec: &stubAnthropic{status: 503, body: `{"error":"down"}`}},
	})

	body, status, err := chain.ExecuteAnthropicRaw(context.Background(), []byte("{}"), nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if status != 503 || !strings.Contains(string(body), "down") {
		t.Fatalf("status=%d body=%s, want the last provider's failure", status, body)
	}
}

// Only providers that speak the protocol are tried, so a chain mixing an
// OpenAI-only provider with an Anthropic one still serves /v1/messages.
func TestChainSkipsProvidersThatCannotSpeakAnthropic(t *testing.T) {
	openAIOnly := &stubExecutorOnly{}
	anthropic := &stubAnthropic{status: 200, body: `{"ok":true}`}
	chain := NewChain([]Link{
		{Provider: "anygen", Exec: openAIOnly},
		{Provider: "relay", Exec: anthropic},
	})

	ctx, getBackend := WithBackendRecorder(context.Background())
	if _, status, err := chain.ExecuteAnthropicRaw(ctx, []byte("{}"), nil); err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if got := getBackend(); got != "relay" {
		t.Errorf("served backend = %q, want relay", got)
	}
}

// A chain always implements ResponsesExecutor so it can forward to whichever
// provider speaks it; asking it directly is the only way to tell whether any
// provider actually does.
func TestChainReportsNativeResponsesSupportHonestly(t *testing.T) {
	openAIOnly := NewChain([]Link{{Provider: "anygen", Exec: &stubExecutorOnly{}}})
	if openAIOnly.SupportsResponses() {
		t.Error("a chain with no Responses provider must not claim native support")
	}
	if _, ok := AsResponsesExecutor(openAIOnly); ok {
		t.Error("AsResponsesExecutor must reject a chain with no Responses provider")
	}
}

// stubExecutorOnly speaks the internal Chat Completions contract and nothing
// else — no Anthropic Messages, no Responses.
type stubExecutorOnly struct{}

func (s *stubExecutorOnly) Models() []string { return []string{"m"} }
func (s *stubExecutorOnly) Execute(context.Context, *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	return &types.ChatCompletionResponse{}, nil
}
func (s *stubExecutorOnly) ExecuteStream(context.Context, *types.ChatCompletionRequest, io.Writer) (*types.Usage, error) {
	return &types.Usage{}, nil
}
