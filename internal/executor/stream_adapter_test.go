package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

// nonStreamingStub behaves like AnyGen: it answers Execute and rejects
// ExecuteStream outright, so any stream request that reaches it is a bug.
type nonStreamingStub struct {
	executeCalls int
	streamCalls  int
	resp         *types.ChatCompletionResponse
	err          error
	gotStream    *bool
}

func (s *nonStreamingStub) Models() []string        { return []string{"m"} }
func (s *nonStreamingStub) SupportsStreaming() bool { return false }
func (s *nonStreamingStub) Execute(_ context.Context, req *types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	s.executeCalls++
	stream := req.Stream
	s.gotStream = &stream
	return s.resp, s.err
}
func (s *nonStreamingStub) ExecuteStream(context.Context, *types.ChatCompletionRequest, io.Writer) (*types.Usage, error) {
	s.streamCalls++
	return nil, &HTTPError{Backend: "stub", Status: http.StatusBadRequest, Body: "non-streaming only"}
}

func parseChunks(t *testing.T, raw string) (chunks []types.ChatCompletionChunk, done bool) {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			done = true
			continue
		}
		var chunk types.ChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", data, err)
		}
		chunks = append(chunks, chunk)
	}
	return chunks, done
}

// The reported bug: relay streams, anygen does not. Relay 429s, the chain moves
// to anygen still carrying stream=true, and anygen rejects it with a 400 that
// the handler turns into a 502. The chain has to notice that the provider it is
// about to try cannot stream and serve it through Execute instead.
func TestChainStreamFailsOverToNonStreamingProvider(t *testing.T) {
	primary := &countingExecutor{err: &HTTPError{Backend: "relay", Status: http.StatusTooManyRequests, Body: "rate limited"}}
	finish := "stop"
	backup := &nonStreamingStub{resp: &types.ChatCompletionResponse{
		ID: "chatcmpl-1", Model: "m",
		Choices: []types.ChatCompletionChoice{{Message: &types.ChatResult{Role: "assistant", Content: "hello"}, FinishReason: &finish}},
		Usage:   &types.Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4},
	}}
	chain := NewChain([]Link{{Provider: "relay", Exec: primary}, {Provider: "anygen", Exec: backup}})

	if !chain.SupportsStreaming() {
		t.Fatal("the head streams, so the chain should advertise streaming for the common path")
	}

	var out strings.Builder
	ctx, getBackend := WithBackendRecorder(context.Background())
	usage, err := chain.ExecuteStream(ctx, &types.ChatCompletionRequest{Model: "m", Stream: true}, &out)
	if err != nil {
		t.Fatalf("ExecuteStream() = %v, want the non-streaming backup to answer", err)
	}
	if backup.streamCalls != 0 {
		t.Fatalf("backup.ExecuteStream called %d times, want 0", backup.streamCalls)
	}
	if backup.executeCalls != 1 {
		t.Fatalf("backup.Execute called %d times, want 1", backup.executeCalls)
	}
	if backup.gotStream == nil || *backup.gotStream {
		t.Fatal("the non-streaming provider must receive stream=false")
	}
	if got := getBackend(); got != "anygen" {
		t.Fatalf("served backend = %q, want anygen", got)
	}
	if usage == nil || usage.TotalTokens != 4 {
		t.Fatalf("usage = %+v, want the completed response's usage", usage)
	}

	chunks, done := parseChunks(t, out.String())
	if !done {
		t.Fatalf("stream missing [DONE]:\n%s", out.String())
	}
	var content string
	var finishReason string
	for _, c := range chunks {
		if c.Object != "chat.completion.chunk" {
			t.Fatalf("chunk object = %q", c.Object)
		}
		for _, ch := range c.Choices {
			if ch.Delta != nil {
				content += ch.Delta.Content
			}
			if ch.FinishReason != nil {
				finishReason = *ch.FinishReason
			}
		}
	}
	if content != "hello" {
		t.Fatalf("content = %q, want hello", content)
	}
	if finishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", finishReason)
	}
	if chunks[len(chunks)-1].Usage == nil {
		t.Fatal("terminal chunk should carry usage")
	}
}

// A non-streaming provider that fails must still hand off to the next link,
// and it must not have written anything first.
func TestChainStreamNonStreamingLinkFailureStillFallsOver(t *testing.T) {
	first := &nonStreamingStub{err: &HTTPError{Backend: "anygen", Status: http.StatusTooManyRequests, Body: "quota"}}
	second := &stubAnthropic{name: "streamed"}
	chain := NewChain([]Link{{Provider: "anygen", Exec: first}, {Provider: "relay", Exec: second}})

	var out strings.Builder
	if _, err := chain.ExecuteStream(context.Background(), &types.ChatCompletionRequest{Model: "m", Stream: true}, &out); err != nil {
		t.Fatalf("ExecuteStream() = %v", err)
	}
	if out.String() != "streamed" {
		t.Fatalf("output = %q, want only the second provider's bytes", out.String())
	}
}

func TestWriteCompletionAsChunkStreamReplaysToolCalls(t *testing.T) {
	finish := "tool_calls"
	resp := &types.ChatCompletionResponse{
		ID: "chatcmpl-1", Model: "m",
		Choices: []types.ChatCompletionChoice{{
			Message: &types.ChatResult{
				Role: "assistant",
				ToolCalls: []types.ToolCall{
					{ID: "call_1", Function: types.ToolCallFunction{Name: "shell", Arguments: `{"cmd":"pwd"}`}},
					{ID: "call_2", Type: "function", Function: types.ToolCallFunction{Name: "read", Arguments: `{"path":"x"}`}},
				},
			},
			FinishReason: &finish,
		}},
	}
	var out strings.Builder
	if err := writeCompletionAsChunkStream(resp, "m", &out); err != nil {
		t.Fatal(err)
	}
	chunks, done := parseChunks(t, out.String())
	if !done {
		t.Fatal("missing [DONE]")
	}
	var calls []types.ToolCall
	var finishReason string
	for _, c := range chunks {
		for _, ch := range c.Choices {
			if ch.Delta != nil {
				calls = append(calls, ch.Delta.ToolCalls...)
			}
			if ch.FinishReason != nil {
				finishReason = *ch.FinishReason
			}
		}
	}
	if len(calls) != 2 {
		t.Fatalf("tool calls = %d, want 2", len(calls))
	}
	if calls[0].Index != 0 || calls[1].Index != 1 {
		t.Fatalf("tool call indexes = %d,%d, want 0,1", calls[0].Index, calls[1].Index)
	}
	if calls[0].Type != "function" || calls[0].ID != "call_1" || calls[0].Function.Arguments != `{"cmd":"pwd"}` {
		t.Fatalf("first tool call = %+v", calls[0])
	}
	if finishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", finishReason)
	}
}
