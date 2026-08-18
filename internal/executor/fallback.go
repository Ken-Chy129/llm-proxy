package executor

import (
	"context"
	"io"
	"log"
	"net/http"
	"sync"

	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

// FallbackExecutor serves a model from primary, and retries on secondary when
// primary reports exhaustion (HTTP 429) or is unreachable.
//
// It exists so a paid relay can act as overflow capacity for OAuth subscription
// accounts: OAuth is tried first and drained, and only a genuine "no quota left"
// answer moves traffic to the relay. Client errors (4xx other than 429) are the
// caller's fault and are passed straight through — retrying them upstream would
// just burn relay budget on a request that cannot succeed.
//
// The primary's own per-account failover runs first and is unaware of this type;
// by the time primary returns 429 it has already tried every OAuth account.
type FallbackExecutor struct {
	primary       Executor
	secondary     Executor
	primaryName   string
	secondaryName string
	models        []string
}

func NewFallbackExecutor(primary, secondary Executor, models []string) *FallbackExecutor {
	return &FallbackExecutor{
		primary:       primary,
		secondary:     secondary,
		primaryName:   "claude",
		secondaryName: "relay",
		models:        models,
	}
}

func (e *FallbackExecutor) Models() []string { return e.models }

// shouldFallOver reports whether a primary result means "primary has no capacity
// for this request". 429 is the quota signal; 5xx and transport errors mean the
// primary could not answer at all. Everything else belongs to the client.
func shouldFallOver(status int, err error) bool {
	if err != nil {
		return true
	}
	return status == http.StatusTooManyRequests || status >= 500
}

func (e *FallbackExecutor) ExecuteAnthropicRaw(ctx context.Context, body []byte, h http.Header) ([]byte, int, error) {
	pa, ok := e.primary.(AnthropicExecutor)
	if !ok {
		return e.secondaryRaw(ctx, body, h)
	}
	recordBackend(ctx, e.primaryName)
	respBody, status, err := pa.ExecuteAnthropicRaw(ctx, body, h)
	if !shouldFallOver(status, err) {
		return respBody, status, err
	}
	log.Printf("[fallback] %s exhausted (status=%d err=%v); retrying on %s", e.primaryName, status, err, e.secondaryName)
	return e.secondaryRaw(ctx, body, h)
}

func (e *FallbackExecutor) secondaryRaw(ctx context.Context, body []byte, h http.Header) ([]byte, int, error) {
	sa, ok := e.secondary.(AnthropicExecutor)
	if !ok {
		return nil, http.StatusBadGateway, errNoAnthropicSecondary
	}
	recordBackend(ctx, e.secondaryName)
	recordBackendFallover(ctx, e.primaryName)
	return sa.ExecuteAnthropicRaw(ctx, body, h)
}

// OpenAnthropicStream can fall over safely because it only opens the upstream
// connection: the status is known before any byte reaches the client, so
// switching backends cannot produce a spliced half-stream.
func (e *FallbackExecutor) OpenAnthropicStream(ctx context.Context, body []byte, h http.Header) (io.ReadCloser, int, error) {
	pa, ok := e.primary.(AnthropicExecutor)
	if !ok {
		return e.secondaryStream(ctx, body, h)
	}
	recordBackend(ctx, e.primaryName)
	stream, status, err := pa.OpenAnthropicStream(ctx, body, h)
	if !shouldFallOver(status, err) {
		return stream, status, err
	}
	if stream != nil {
		stream.Close()
	}
	log.Printf("[fallback] %s exhausted (status=%d err=%v); retrying on %s", e.primaryName, status, err, e.secondaryName)
	return e.secondaryStream(ctx, body, h)
}

func (e *FallbackExecutor) secondaryStream(ctx context.Context, body []byte, h http.Header) (io.ReadCloser, int, error) {
	sa, ok := e.secondary.(AnthropicExecutor)
	if !ok {
		return nil, http.StatusBadGateway, errNoAnthropicSecondary
	}
	recordBackend(ctx, e.secondaryName)
	recordBackendFallover(ctx, e.primaryName)
	return sa.OpenAnthropicStream(ctx, body, h)
}

func (e *FallbackExecutor) Execute(ctx context.Context, req *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	recordBackend(ctx, e.primaryName)
	resp, err := e.primary.Execute(ctx, req)
	if err == nil {
		return resp, nil
	}
	if !shouldFallOver(StatusFromError(err), nil) && StatusFromError(err) != 0 {
		return resp, err
	}
	log.Printf("[fallback] %s exhausted (%v); retrying on %s", e.primaryName, err, e.secondaryName)
	recordBackend(ctx, e.secondaryName)
	recordBackendFallover(ctx, e.primaryName)
	return e.secondary.Execute(ctx, req)
}

// ExecuteStream buffers nothing, so a mid-stream primary failure cannot be
// retried without duplicating already-sent bytes. Only a failure to start is
// eligible for fallback, which is what writeSeen tracks.
func (e *FallbackExecutor) ExecuteStream(ctx context.Context, req *types.ChatCompletionRequest, w io.Writer) (*types.Usage, error) {
	tracked := &firstWriteTracker{w: w}
	recordBackend(ctx, e.primaryName)
	usage, err := e.primary.ExecuteStream(ctx, req, tracked)
	if err == nil || tracked.wrote() {
		return usage, err
	}
	if st := StatusFromError(err); st != 0 && !shouldFallOver(st, nil) {
		return usage, err
	}
	log.Printf("[fallback] %s exhausted (%v); retrying on %s", e.primaryName, err, e.secondaryName)
	recordBackend(ctx, e.secondaryName)
	recordBackendFallover(ctx, e.primaryName)
	return e.secondary.ExecuteStream(ctx, req, w)
}

type firstWriteTracker struct {
	w       io.Writer
	mu      sync.Mutex
	written bool
}

func (t *firstWriteTracker) Write(p []byte) (int, error) {
	if len(p) > 0 {
		t.mu.Lock()
		t.written = true
		t.mu.Unlock()
	}
	return t.w.Write(p)
}

func (t *firstWriteTracker) wrote() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.written
}
