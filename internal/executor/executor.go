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

type ResponsesExecutor interface {
	OpenResponsesStream(ctx context.Context, body []byte) (io.ReadCloser, error)
}

type AnthropicExecutor interface {
	ExecuteAnthropicRaw(ctx context.Context, body []byte, clientHeaders http.Header) ([]byte, int, error)
	OpenAnthropicStream(ctx context.Context, body []byte, clientHeaders http.Header) (io.ReadCloser, int, error)
}
