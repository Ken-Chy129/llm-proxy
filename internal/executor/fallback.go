package executor

import (
	"context"
	"io"
	"log"
	"net/http"
	"sync"

	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

// Link is one provider in a chain: the name to record and the executor to call.
type Link struct {
	Provider string
	Exec     Executor
}

// Chain serves a model from an ordered list of providers, moving to the next
// one when the current provider reports exhaustion (HTTP 429) or cannot answer
// at all (5xx, transport failure).
//
// The order comes straight from the model's configured provider chain, so the
// routing preference and the failover order are the same thing: a paid relay
// listed behind an OAuth subscription is overflow capacity, tried only once the
// subscription answers "no quota left".
//
// Client errors (4xx other than 429) are the caller's fault and are passed
// straight through — retrying them elsewhere would just spend another
// provider's budget on a request that cannot succeed.
//
// Per-account failover inside a provider runs first and is unaware of this type;
// by the time a provider returns 429 it has already tried each of its accounts.
type Chain struct {
	links []Link
}

// NewChain builds a chain over links, which must be non-empty and already
// filtered to providers that can serve the request.
func NewChain(links []Link) *Chain {
	return &Chain{links: links}
}

// Providers lists the chain's providers in order.
func (e *Chain) Providers() []string {
	out := make([]string, len(e.links))
	for i, l := range e.links {
		out[i] = l.Provider
	}
	return out
}

// Models is unused for a chain — the router owns the model table — but Executor
// requires it.
func (e *Chain) Models() []string { return nil }

// SupportsStreaming reports what the first link supports. Handlers use it to
// pick an adaptation strategy before the request starts, so it can only speak
// for the provider that will actually be tried first.
func (e *Chain) SupportsStreaming() bool {
	if s, ok := e.links[0].Exec.(StreamingSupport); ok {
		return s.SupportsStreaming()
	}
	return true
}

// shouldFallOver reports whether a primary result means "primary has no capacity
// for this request". 429 is the quota signal; 5xx and transport errors mean the
// primary could not answer at all. Everything else belongs to the client.
func shouldFallOver(status int, err error) bool {
	if err != nil {
		return true
	}
	return status == http.StatusTooManyRequests || status >= 500
}

// ExecuteAnthropicRaw walks the chain, skipping providers that cannot speak the
// Messages protocol.
func (e *Chain) ExecuteAnthropicRaw(ctx context.Context, body []byte, h http.Header) ([]byte, int, error) {
	var lastBody []byte
	var lastStatus int
	var lastErr error
	tried := false
	for i, link := range e.links {
		pa, ok := link.Exec.(AnthropicExecutor)
		if !ok {
			continue
		}
		if tried {
			recordBackendFallover(ctx, e.links[i-1].Provider)
		}
		tried = true
		recordBackend(ctx, link.Provider)
		lastBody, lastStatus, lastErr = pa.ExecuteAnthropicRaw(ctx, body, h)
		if !shouldFallOver(lastStatus, lastErr) {
			return lastBody, lastStatus, lastErr
		}
		if next, ok := e.nextProvider(i, true); ok {
			log.Printf("[chain] %s exhausted (status=%d err=%v); retrying on %s", link.Provider, lastStatus, lastErr, next)
		}
	}
	if !tried {
		return nil, http.StatusBadGateway, errNoAnthropicProvider
	}
	return lastBody, lastStatus, lastErr
}

// OpenAnthropicStream can fall over safely because it only opens the upstream
// connection: the status is known before any byte reaches the client, so
// switching providers cannot produce a spliced half-stream.
func (e *Chain) OpenAnthropicStream(ctx context.Context, body []byte, h http.Header) (io.ReadCloser, int, error) {
	var lastStream io.ReadCloser
	var lastStatus int
	var lastErr error
	tried := false
	for i, link := range e.links {
		pa, ok := link.Exec.(AnthropicExecutor)
		if !ok {
			continue
		}
		if tried {
			recordBackendFallover(ctx, e.links[i-1].Provider)
		}
		tried = true
		recordBackend(ctx, link.Provider)
		lastStream, lastStatus, lastErr = pa.OpenAnthropicStream(ctx, body, h)
		if !shouldFallOver(lastStatus, lastErr) {
			return lastStream, lastStatus, lastErr
		}
		if lastStream != nil {
			lastStream.Close()
			lastStream = nil
		}
		if next, ok := e.nextProvider(i, true); ok {
			log.Printf("[chain] %s exhausted (status=%d err=%v); retrying on %s", link.Provider, lastStatus, lastErr, next)
		}
	}
	if !tried {
		return nil, http.StatusBadGateway, errNoAnthropicProvider
	}
	return lastStream, lastStatus, lastErr
}

// nextProvider names the provider a failure at index i would move to, so the
// log line can say where the request is actually going.
func (e *Chain) nextProvider(i int, anthropicOnly bool) (string, bool) {
	for _, link := range e.links[i+1:] {
		if anthropicOnly {
			if _, ok := link.Exec.(AnthropicExecutor); !ok {
				continue
			}
		}
		return link.Provider, true
	}
	return "", false
}

func (e *Chain) Execute(ctx context.Context, req *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	var resp *types.ChatCompletionResponse
	var err error
	for i, link := range e.links {
		if i > 0 {
			recordBackendFallover(ctx, e.links[i-1].Provider)
		}
		recordBackend(ctx, link.Provider)
		resp, err = link.Exec.Execute(ctx, req)
		if err == nil {
			return resp, nil
		}
		status := StatusFromError(err)
		if !shouldFallOver(status, nil) && status != 0 {
			return resp, err
		}
		if next, ok := e.nextProvider(i, false); ok {
			log.Printf("[chain] %s exhausted (%v); retrying on %s", link.Provider, err, next)
		}
	}
	return resp, err
}

// ExecuteStream buffers nothing, so a mid-stream failure cannot be retried
// without duplicating already-sent bytes. Only a failure to start is eligible
// for failover, which is what firstWriteTracker detects.
func (e *Chain) ExecuteStream(ctx context.Context, req *types.ChatCompletionRequest, w io.Writer) (*types.Usage, error) {
	var usage *types.Usage
	var err error
	for i, link := range e.links {
		if i > 0 {
			recordBackendFallover(ctx, e.links[i-1].Provider)
		}
		recordBackend(ctx, link.Provider)
		tracked := &firstWriteTracker{w: w}
		usage, err = link.Exec.ExecuteStream(ctx, req, tracked)
		if err == nil || tracked.wrote() {
			return usage, err
		}
		if st := StatusFromError(err); st != 0 && !shouldFallOver(st, nil) {
			return usage, err
		}
		if next, ok := e.nextProvider(i, false); ok {
			log.Printf("[chain] %s exhausted (%v); retrying on %s", link.Provider, err, next)
		}
	}
	return usage, err
}

// SupportsResponses reports whether any provider in the chain speaks the native
// Responses protocol. A chain always *implements* ResponsesExecutor, so callers
// must ask this rather than type-asserting; see AsResponsesExecutor.
func (e *Chain) SupportsResponses() bool {
	for _, link := range e.links {
		if _, ok := link.Exec.(ResponsesExecutor); ok {
			return true
		}
	}
	return false
}

// OpenResponsesStream forwards the native Responses protocol. Like the
// Anthropic stream, the upstream status is known before any byte reaches the
// client, so moving to the next provider is safe.
func (e *Chain) OpenResponsesStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	var lastErr error
	tried := false
	for i, link := range e.links {
		re, ok := link.Exec.(ResponsesExecutor)
		if !ok {
			continue
		}
		if tried {
			recordBackendFallover(ctx, e.links[i-1].Provider)
		}
		tried = true
		recordBackend(ctx, link.Provider)
		stream, err := re.OpenResponsesStream(ctx, body)
		if err == nil {
			return stream, nil
		}
		lastErr = err
		status := StatusFromError(err)
		if !shouldFallOver(status, nil) && status != 0 {
			return nil, err
		}
	}
	if !tried {
		return nil, errNoResponsesProvider
	}
	return nil, lastErr
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
