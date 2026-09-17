package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

// Returned when a chain is asked for a protocol none of its providers speak.
// The router filters by protocol before building a chain, so these indicate a
// routing bug rather than a configuration mistake.
var (
	errNoAnthropicProvider = errors.New("no provider in the chain supports the Anthropic Messages API")
	errNoResponsesProvider = errors.New("no provider in the chain supports the Responses API")
)

// HTTPError carries the upstream HTTP status code alongside the error, so
// handlers can record the real status (429/400/…) instead of a blanket 500.
type HTTPError struct {
	Backend string
	Status  int
	Body    string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s error %d: %s", e.Backend, e.Status, e.Body)
}

// StatusFromError returns the upstream HTTP status wrapped in an *HTTPError, or
// 0 when the error carries none (connection failures, timeouts, etc.).
func StatusFromError(err error) int {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status
	}
	return 0
}

type accountRecorder struct {
	mu         sync.Mutex
	account    string   // the account that ultimately served (or last tried)
	failedOver []string // accounts that 429'd and were skipped before the final one
}

type ctxAccountKey struct{}

type backendRecorder struct {
	mu         sync.Mutex
	backend    string
	failedOver []string
}

type ctxBackendKey struct{}

type attemptRecorder struct {
	mu       sync.Mutex
	attempts []types.FailureAttempt
}

type ctxAttemptKey struct{}

type artifactRecorder struct {
	mu        sync.Mutex
	artifacts []types.DiagnosticArtifact
}

type ctxArtifactKey struct{}

// WithAttemptRecorder captures every failed account/provider try in order.
func WithAttemptRecorder(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, ctxAttemptKey{}, &attemptRecorder{})
	return context.WithValue(ctx, ctxArtifactKey{}, &artifactRecorder{})
}

// FailureAttempts returns a defensive copy of the request's failed tries.
func FailureAttempts(ctx context.Context) []types.FailureAttempt {
	if r, ok := ctx.Value(ctxAttemptKey{}).(*attemptRecorder); ok {
		r.mu.Lock()
		defer r.mu.Unlock()
		return append([]types.FailureAttempt(nil), r.attempts...)
	}
	return nil
}

// DiagnosticArtifacts returns request-scoped provider evidence for incident
// capture. It is never exposed through the logs API.
func DiagnosticArtifacts(ctx context.Context) []types.DiagnosticArtifact {
	if r, ok := ctx.Value(ctxArtifactKey{}).(*artifactRecorder); ok {
		r.mu.Lock()
		defer r.mu.Unlock()
		out := make([]types.DiagnosticArtifact, len(r.artifacts))
		for i, artifact := range r.artifacts {
			out[i] = types.DiagnosticArtifact{Name: artifact.Name, Data: append([]byte(nil), artifact.Data...)}
		}
		return out
	}
	return nil
}

func recordDiagnosticArtifact(ctx context.Context, name string, data []byte) {
	const maxArtifactBytes = 32 << 20
	if len(data) > maxArtifactBytes {
		data = data[:maxArtifactBytes]
	}
	if r, ok := ctx.Value(ctxArtifactKey{}).(*artifactRecorder); ok {
		r.mu.Lock()
		r.artifacts = append(r.artifacts, types.DiagnosticArtifact{Name: name, Data: append([]byte(nil), data...)})
		r.mu.Unlock()
	}
}

func recordFailureAttempt(ctx context.Context, attempt types.FailureAttempt) {
	const maxAttemptError = 2048
	if len(attempt.Error) > maxAttemptError {
		attempt.Error = attempt.Error[:maxAttemptError] + "…"
	}
	if r, ok := ctx.Value(ctxAttemptKey{}).(*attemptRecorder); ok {
		r.mu.Lock()
		r.attempts = append(r.attempts, attempt)
		r.mu.Unlock()
	}
}

func currentAccount(ctx context.Context) string {
	if r, ok := ctx.Value(ctxAccountKey{}).(*accountRecorder); ok {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.account
	}
	return ""
}

func recordAccountFailure(ctx context.Context, provider, account string, status int, err error) {
	message := "upstream attempt failed"
	if err != nil {
		message = err.Error()
	}
	recordFailureAttempt(ctx, types.FailureAttempt{
		Scope: "account", Provider: provider, Account: account, Status: status, Error: message,
	})
}

func recordProviderFailure(ctx context.Context, provider string, status int, err error) {
	message := "upstream attempt failed"
	if err != nil {
		message = err.Error()
	}
	recordFailureAttempt(ctx, types.FailureAttempt{
		Scope: "provider", Provider: provider, Account: currentAccount(ctx), Status: status, Error: message,
	})
}

// WithBackendRecorder returns a derived context that captures which backend
// actually served the request, which differs from the routing table entry when a
// fallback chain moves traffic to its secondary. The getter reports the serving
// backend, or "" when nothing recorded.
func WithBackendRecorder(ctx context.Context) (context.Context, func() string) {
	r := &backendRecorder{}
	ctx = context.WithValue(ctx, ctxBackendKey{}, r)
	return ctx, func() string {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.backend
	}
}

// BackendFallbackFrom reports the backends that were exhausted before the
// serving one, in order. Empty when no fallback happened.
func BackendFallbackFrom(ctx context.Context) []string {
	if r, ok := ctx.Value(ctxBackendKey{}).(*backendRecorder); ok {
		r.mu.Lock()
		defer r.mu.Unlock()
		return append([]string(nil), r.failedOver...)
	}
	return nil
}

// ServingBackend reports the provider that handled the request, or "" when the
// context carries no recorder or nothing ran yet.
func ServingBackend(ctx context.Context) string {
	if r, ok := ctx.Value(ctxBackendKey{}).(*backendRecorder); ok {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.backend
	}
	return ""
}

func recordBackend(ctx context.Context, backend string) {
	if r, ok := ctx.Value(ctxBackendKey{}).(*backendRecorder); ok {
		r.mu.Lock()
		r.backend = backend
		r.mu.Unlock()
	}
}

func recordBackendFallover(ctx context.Context, backend string) {
	if r, ok := ctx.Value(ctxBackendKey{}).(*backendRecorder); ok {
		r.mu.Lock()
		r.failedOver = append(r.failedOver, backend)
		r.mu.Unlock()
	}
}

// WithAccountRecorder returns a derived context that captures which upstream
// account an executor selects while handling the request, plus a getter to read
// the result afterwards (for request logging): the serving account, and the
// ordered list of accounts that were rate-limited and failed over from. Both are
// empty if the executor never recorded anything.
func WithAccountRecorder(ctx context.Context) (context.Context, func() (string, []string)) {
	r := &accountRecorder{}
	ctx = context.WithValue(ctx, ctxAccountKey{}, r)
	return ctx, func() (string, []string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.account, r.failedOver
	}
}

// recordAccount notes the upstream account used for this request. No-op when the
// context carries no recorder.
func recordAccount(ctx context.Context, account string) {
	if r, ok := ctx.Value(ctxAccountKey{}).(*accountRecorder); ok {
		r.mu.Lock()
		r.account = account
		r.mu.Unlock()
	}
}

// recordAccountFailover notes that an account was rate-limited and the request
// is failing over to another account. No-op when the context carries no recorder.
func recordAccountFailover(ctx context.Context, account string) {
	if r, ok := ctx.Value(ctxAccountKey{}).(*accountRecorder); ok {
		r.mu.Lock()
		r.failedOver = append(r.failedOver, account)
		r.mu.Unlock()
	}
}

type Executor interface {
	Execute(ctx context.Context, req *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error)
	ExecuteStream(ctx context.Context, req *types.ChatCompletionRequest, w io.Writer) (*types.Usage, error)
	Models() []string
}

// StreamingSupport is optional. Executors that omit it are assumed to support
// streaming for backward compatibility. A Chain never forwards a stream request
// to an executor that returns false: it calls Execute instead and replays the
// completed result as Chat Completions chunks, so the decision is made per
// provider at the moment that provider is actually tried — including after a
// failover from a provider that does stream.
type StreamingSupport interface {
	SupportsStreaming() bool
}

type ResponsesExecutor interface {
	OpenResponsesStream(ctx context.Context, body []byte) (io.ReadCloser, error)
}

type AnthropicExecutor interface {
	ExecuteAnthropicRaw(ctx context.Context, body []byte, clientHeaders http.Header) ([]byte, int, error)
	OpenAnthropicStream(ctx context.Context, body []byte, clientHeaders http.Header) (io.ReadCloser, int, error)
}

// AsResponsesExecutor reports whether exec can serve the native Responses
// protocol, and returns it if so.
//
// A plain type assertion is not enough: a Chain implements ResponsesExecutor
// unconditionally in order to forward to whichever of its providers speaks it,
// so asserting alone would claim native support for a chain of providers that
// have none. Executors answer for themselves; a chain answers for its links.
func AsResponsesExecutor(exec Executor) (ResponsesExecutor, bool) {
	re, ok := exec.(ResponsesExecutor)
	if !ok {
		return nil, false
	}
	if c, isChain := exec.(*Chain); isChain && !c.SupportsResponses() {
		return nil, false
	}
	return re, true
}
