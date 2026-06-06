package executor

import (
	"context"
	"io"
	"net/http"

	"github.com/user/cli-proxy/internal/types"
)

type Executor interface {
	Execute(ctx context.Context, req *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error)
	ExecuteStream(ctx context.Context, req *types.ChatCompletionRequest, w io.Writer) error
	Models() []string
}

type ResponsesExecutor interface {
	OpenResponsesStream(ctx context.Context, body []byte) (io.ReadCloser, error)
}

type AnthropicExecutor interface {
	ExecuteAnthropicRaw(ctx context.Context, body []byte, clientHeaders http.Header) ([]byte, int, error)
	OpenAnthropicStream(ctx context.Context, body []byte, clientHeaders http.Header) (io.ReadCloser, int, error)
}
