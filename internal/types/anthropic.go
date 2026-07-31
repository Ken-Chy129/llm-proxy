package types

import "encoding/json"

type AnthropicRequest struct {
	Model         string             `json:"model,omitempty"`
	Messages      []AnthropicMessage `json:"messages"`
	System        string             `json:"system,omitempty"`
	MaxTokens     int                `json:"max_tokens"`
	Stream        bool               `json:"stream,omitempty"`
	Thinking      *ThinkingConfig    `json:"thinking,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Tools         []AnthropicTool    `json:"tools,omitempty"`

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
