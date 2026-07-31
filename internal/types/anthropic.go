package types

import "encoding/json"

// CacheControl marks a prompt-caching breakpoint. Anthropic caches the prefix up
// to and including the block that carries it, so placement decides what gets
// reused; "ephemeral" (5-minute TTL) is the only kind the API accepts today.
//
// Caching is opt-in on the wire: a request without a single cache_control is
// treated as cold no matter how much of the prompt repeats.
type CacheControl struct {
	Type string `json:"type"`
}

// Ephemeral returns the only breakpoint kind Anthropic currently accepts.
func Ephemeral() *CacheControl { return &CacheControl{Type: "ephemeral"} }

// AnthropicSystemBlock is one block of the system prompt. System has to be an
// array rather than a plain string: a string has nowhere to hang cache_control,
// which is what made every translated request uncacheable.
type AnthropicSystemBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type AnthropicRequest struct {
	Model         string                 `json:"model,omitempty"`
	Messages      []AnthropicMessage     `json:"messages"`
	System        []AnthropicSystemBlock `json:"system,omitempty"`
	MaxTokens     int                    `json:"max_tokens"`
	Stream        bool                   `json:"stream,omitempty"`
	Thinking      *ThinkingConfig        `json:"thinking,omitempty"`
	Temperature   *float64               `json:"temperature,omitempty"`
	TopP          *float64               `json:"top_p,omitempty"`
	StopSequences []string               `json:"stop_sequences,omitempty"`
	Tools         []AnthropicTool        `json:"tools,omitempty"`

	AnthropicVersion string `json:"anthropic_version,omitempty"`
}

type ThinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type AnthropicContentBlock struct {
	Type      string                `json:"type"`
	Text      string                `json:"text,omitempty"`
	ID        string                `json:"id,omitempty"`
	Name      string                `json:"name,omitempty"`
	Input     json.RawMessage       `json:"input,omitempty"`
	ToolUseID string                `json:"tool_use_id,omitempty"`
	Content   string                `json:"content,omitempty"`
	Source    *AnthropicMediaSource `json:"source,omitempty"`
	// CacheControl on the last block of the last message is how a growing
	// conversation gets its prefix cached.
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// AnthropicMediaSource is the payload of an image or document content block.
// Type is "base64" (with MediaType + Data) or "url" (with URL).
type AnthropicMediaSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	// CacheControl on the last tool caches the whole toolset. That prefix is
	// identical across every request from one client, so it survives even between
	// unrelated conversations — the cheapest breakpoint there is.
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type AnthropicResponse struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Content      []AnthropicContentBlock `json:"content"`
	Model        string                  `json:"model"`
	StopReason   string                  `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	Usage        AnthropicUsage          `json:"usage"`
}

// AnthropicUsage mirrors the Messages API usage object. input_tokens here
// excludes both cache buckets — the three input fields are disjoint and only
// sum to the real prompt size when added together.
type AnthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// MergeNonZero folds a later usage object into u, keeping already-populated
// fields. Anthropic splits usage across message_start (input + cache) and
// message_delta (output, and in newer API versions the full object again), so a
// plain overwrite would let the delta's zeroed input fields erase the real
// numbers from message_start.
func (u *AnthropicUsage) MergeNonZero(next AnthropicUsage) {
	if next.InputTokens != 0 {
		u.InputTokens = next.InputTokens
	}
	if next.OutputTokens != 0 {
		u.OutputTokens = next.OutputTokens
	}
	if next.CacheCreationInputTokens != 0 {
		u.CacheCreationInputTokens = next.CacheCreationInputTokens
	}
	if next.CacheReadInputTokens != 0 {
		u.CacheReadInputTokens = next.CacheReadInputTokens
	}
}

type AnthropicStreamEvent struct {
	Type  string          `json:"type"`
	Index int             `json:"index,omitempty"`
	Delta json.RawMessage `json:"delta,omitempty"`

	ContentBlock *AnthropicContentBlock `json:"content_block,omitempty"`

	Message *AnthropicResponse `json:"message,omitempty"`
	Usage   *AnthropicUsage    `json:"usage,omitempty"`
}
