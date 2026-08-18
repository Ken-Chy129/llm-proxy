package executor

import (
	"context"
	"io"
	"net/http"
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
	fb := NewFallbackExecutor(primary, secondary, []string{"m"})

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
	fb := NewFallbackExecutor(primary, secondary, []string{"m"})

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
	fb := NewFallbackExecutor(primary, secondary, []string{"m"})

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
	fb := NewFallbackExecutor(primary, secondary, []string{"m"})

	ctx, getBackend := WithBackendRecorder(context.Background())
	if _, _, err := fb.ExecuteAnthropicRaw(ctx, []byte("{}"), nil); err != nil {
		t.Fatalf("err=%v", err)
	}
	if got := getBackend(); got != "relay" {
		t.Fatalf("served backend = %q, want relay", got)
	}
}
