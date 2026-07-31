package types

import "encoding/json"

type ChatCompletionRequest struct {
	Model           string          `json:"model"`
	Messages        []ChatMessage   `json:"messages"`
	Stream          bool            `json:"stream,omitempty"`
	MaxTokens       int             `json:"max_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Stop            json.RawMessage `json:"stop,omitempty"`
	Tools           []Tool          `json:"tools,omitempty"`
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
	ResponseFormat  *ResponseFormat `json:"response_format,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
}

type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type ToolCall struct {
	Index    int              `json:"index,omitempty"`
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ResponseFormat struct {
	Type string `json:"type"`
}

type ChatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []ChatCompletionChoice `json:"choices"`
	Usage   *Usage                 `json:"usage,omitempty"`
}

type ChatCompletionChoice struct {
	Index        int         `json:"index"`
	Message      *ChatResult `json:"message,omitempty"`
	Delta        *ChatResult `json:"delta,omitempty"`
	FinishReason *string     `json:"finish_reason"`
}

type ChatResult struct {
	Role             string     `json:"role,omitempty"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// Usage keeps OpenAI wire semantics: PromptTokens *includes* cached tokens, and
// PromptTokensDetails.CachedTokens is the subset of it. Call Breakdown() to get
// the disjoint accounting used for storage — do not hand-roll the subtraction.
//
// The details structs are pointers with omitempty so a nil breakdown serialises
// exactly as before for clients that predate them.
type Usage struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	TotalTokens             int                      `json:"total_tokens"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
	// CacheWriteTokens carries Anthropic's cache_creation_input_tokens. OpenAI
	// has no equivalent (its automatic caching charges nothing to write), so
	// this is a proxy extension and stays omitted on OpenAI-served requests.
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// SetBreakdown writes a canonical TokenUsage back into OpenAI wire shape,
// re-adding the cached subset into PromptTokens. Used by executors that read an
// Anthropic upstream but must return an OpenAI-shaped usage object.
func (u *Usage) SetBreakdown(b TokenUsage) {
	u.PromptTokens = b.Input + b.CacheRead + b.CacheWrite
	u.CompletionTokens = b.Output
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	if b.CacheRead != 0 || b.CacheWrite != 0 {
		u.PromptTokensDetails = &PromptTokensDetails{
			CachedTokens:     b.CacheRead,
			CacheWriteTokens: b.CacheWrite,
		}
	}
	if b.Reasoning != ReasoningUnknown {
		u.CompletionTokensDetails = &CompletionTokensDetails{ReasoningTokens: b.Reasoning}
	}
}

type ChatCompletionChunk struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []ChatCompletionChoice `json:"choices"`
	Usage   *Usage                 `json:"usage,omitempty"`
}
