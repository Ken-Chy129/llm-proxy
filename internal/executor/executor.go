package executor

import (
	"context"
	"io"

	"github.com/user/cli-proxy/internal/types"
)

type Executor interface {
	Execute(ctx context.Context, req *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error)
	ExecuteStream(ctx context.Context, req *types.ChatCompletionRequest, w io.Writer) error
	Models() []string
}
