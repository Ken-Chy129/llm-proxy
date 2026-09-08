package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

// supportsStreaming answers for a single executor. Executors that do not
// implement StreamingSupport are assumed to stream.
func supportsStreaming(exec Executor) bool {
	if s, ok := exec.(StreamingSupport); ok {
		return s.SupportsStreaming()
	}
	return true
}

// executeAsStream serves a streaming request on an executor that can only
// answer non-streaming: it runs Execute and replays the completed result as the
// Chat Completions chunk sequence a streaming client expects.
//
// Nothing reaches w until Execute has succeeded, so an upstream failure here
// looks exactly like a failure to open a real stream and stays eligible for
// failover.
func executeAsStream(ctx context.Context, exec Executor, req *types.ChatCompletionRequest, w io.Writer) (*types.Usage, error) {
	nonStreaming := *req
	nonStreaming.Stream = false
	resp, err := exec.Execute(ctx, &nonStreaming)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("non-streaming executor returned an empty response")
	}
	if err := writeCompletionAsChunkStream(resp, req.Model, w); err != nil {
		return resp.Usage, err
	}
	return resp.Usage, nil
}

// writeCompletionAsChunkStream emits one completed Chat Completions response as
// SSE chunks: a role chunk, the content and tool calls, then a terminal chunk
// that carries finish_reason and usage, followed by [DONE].
//
// Tool calls are sent whole (id, name and full arguments in one chunk), which
// is a legal streaming shape: clients accumulate arguments per call and treat a
// chunk that carries an id as the start of a new call.
func writeCompletionAsChunkStream(resp *types.ChatCompletionResponse, requestedModel string, w io.Writer) error {
	model := resp.Model
	if model == "" {
		model = requestedModel
	}
	chunkID := resp.ID
	if chunkID == "" {
		chunkID = "chatcmpl-adapted"
	}
	base := func() types.ChatCompletionChunk {
		return types.ChatCompletionChunk{
			ID: chunkID, Object: "chat.completion.chunk", Created: resp.Created, Model: model,
		}
	}
	emit := func(choice types.ChatCompletionChoice, usage *types.Usage) error {
		chunk := base()
		chunk.Choices = []types.ChatCompletionChoice{choice}
		chunk.Usage = usage
		return writeSSEChunkErr(w, chunk)
	}

	for i, choice := range resp.Choices {
		msg := choice.Message
		if msg == nil {
			msg = &types.ChatResult{}
		}
		role := msg.Role
		if role == "" {
			role = "assistant"
		}
		if err := emit(types.ChatCompletionChoice{Index: choice.Index, Delta: &types.ChatResult{Role: role}}, nil); err != nil {
			return err
		}
		if msg.ReasoningContent != "" {
			if err := emit(types.ChatCompletionChoice{Index: choice.Index, Delta: &types.ChatResult{ReasoningContent: msg.ReasoningContent}}, nil); err != nil {
				return err
			}
		}
		if msg.Content != "" {
			if err := emit(types.ChatCompletionChoice{Index: choice.Index, Delta: &types.ChatResult{Content: msg.Content}}, nil); err != nil {
				return err
			}
		}
		for j, tc := range msg.ToolCalls {
			tc.Index = j
			if tc.Type == "" {
				tc.Type = "function"
			}
			if err := emit(types.ChatCompletionChoice{Index: choice.Index, Delta: &types.ChatResult{ToolCalls: []types.ToolCall{tc}}}, nil); err != nil {
				return err
			}
		}

		finishReason := "stop"
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			finishReason = *choice.FinishReason
		} else if len(msg.ToolCalls) > 0 {
			finishReason = "tool_calls"
		}
		var usage *types.Usage
		if i == len(resp.Choices)-1 {
			usage = resp.Usage
		}
		if err := emit(types.ChatCompletionChoice{Index: choice.Index, Delta: &types.ChatResult{}, FinishReason: &finishReason}, usage); err != nil {
			return err
		}
	}
	if len(resp.Choices) == 0 {
		// A response with no choices still has to terminate the stream cleanly
		// so the client does not hang waiting for a finish reason.
		finishReason := "stop"
		if err := emit(types.ChatCompletionChoice{Index: 0, Delta: &types.ChatResult{}, FinishReason: &finishReason}, resp.Usage); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return err
}

func writeSSEChunkErr(w io.Writer, chunk types.ChatCompletionChunk) error {
	data, err := json.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("marshal chat completion chunk: %w", err)
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}
