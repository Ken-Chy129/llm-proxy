package executor

import (
	"context"
	"net/http"
	"strings"
	"sync"
)

// Codex's own client sends a small set of session headers alongside the body
// and expects some of them echoed back by upstream on the next turn. The most
// important is x-codex-turn-state, which upstream returns in the response and
// the client replays on the following request so that a conversation keeps
// landing on the machine holding its prompt cache. Dropping these headers
// still produces correct responses, but cache routing then has nothing except
// the prefix hash to work with, and under load the session prefix is lost.
//
// Only headers that carry session or routing state are forwarded. Auth,
// User-Agent and content negotiation stay under the proxy's control.
var codexForwardedHeaderPrefixes = []string{"x-codex-"}

var codexForwardedHeaders = map[string]bool{
	"session_id":          true,
	"chatgpt-account-id":  true,
	"x-oai-attestation":   true,
	"x-client-request-id": true,
	"openai-beta":         false, // proxy sets its own
}

// codexExposedResponseHeaders are the upstream response headers returned to the
// client. x-codex-turn-state is the routing token; the rest are diagnostics.
var codexExposedResponseHeaders = []string{
	"x-codex-turn-state",
	"x-request-id",
	"openai-model",
	"x-reasoning-included",
	"openai-processing-ms",
}

// CodexClientHeaderForwardable reports whether a client header should reach
// the Codex upstream unchanged. Never forwards Authorization or anything the
// executor sets itself.
func CodexClientHeaderForwardable(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "authorization", "content-type", "accept", "user-agent", "content-length",
		"host", "accept-encoding", "x-codex-installation-id", "x-openai-internal-codex-residency":
		return false
	}
	if v, ok := codexForwardedHeaders[lower]; ok {
		return v
	}
	for _, p := range codexForwardedHeaderPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

type ctxClientHeadersKey struct{}

type upstreamHeaderRecorder struct {
	mu      sync.Mutex
	headers http.Header
}

type ctxUpstreamHeadersKey struct{}

// WithClientHeaders attaches the incoming request headers so a native
// Responses executor can forward the session-scoped subset upstream.
func WithClientHeaders(ctx context.Context, h http.Header) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxClientHeadersKey{}, h.Clone())
}

func clientHeaders(ctx context.Context) http.Header {
	h, _ := ctx.Value(ctxClientHeadersKey{}).(http.Header)
	return h
}

// applyCodexClientHeaders copies the forwardable client headers onto an
// upstream request. Headers the executor already set take precedence.
func applyCodexClientHeaders(ctx context.Context, req *http.Request) int {
	src := clientHeaders(ctx)
	if src == nil {
		return 0
	}
	copied := 0
	for name, values := range src {
		if !CodexClientHeaderForwardable(name) || req.Header.Get(name) != "" {
			continue
		}
		for _, v := range values {
			req.Header.Add(name, v)
		}
		copied++
	}
	return copied
}

// WithUpstreamHeaderRecorder lets the handler read the exposed upstream response
// headers once the executor has opened the stream.
func WithUpstreamHeaderRecorder(ctx context.Context) (context.Context, func() http.Header) {
	r := &upstreamHeaderRecorder{}
	ctx = context.WithValue(ctx, ctxUpstreamHeadersKey{}, r)
	return ctx, func() http.Header {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.headers == nil {
			return nil
		}
		return r.headers.Clone()
	}
}

func recordUpstreamHeaders(ctx context.Context, h http.Header) {
	r, ok := ctx.Value(ctxUpstreamHeadersKey{}).(*upstreamHeaderRecorder)
	if !ok || h == nil {
		return
	}
	out := make(http.Header)
	for _, name := range codexExposedResponseHeaders {
		if v := h.Values(name); len(v) > 0 {
			out[http.CanonicalHeaderKey(name)] = append([]string(nil), v...)
		}
	}
	// A failed attempt must not leave a stale turn-state from another account.
	r.mu.Lock()
	r.headers = out
	r.mu.Unlock()
}

// ApplyCodexClientHeadersForTest and RecordUpstreamHeadersForTest expose the
// header plumbing to handler tests that stand in for the Codex executor.
func ApplyCodexClientHeadersForTest(ctx context.Context, req *http.Request) {
	applyCodexClientHeaders(ctx, req)
}

func RecordUpstreamHeadersForTest(ctx context.Context, h http.Header) {
	recordUpstreamHeaders(ctx, h)
}
