package types

import (
	"encoding/json"
	"fmt"
	"strings"
)

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

// HasUsableAssistantOutput reports whether adapting this response can produce
// at least one visible text item or function call.
func (r *ChatCompletionResponse) HasUsableAssistantOutput() bool {
	if r == nil {
		return false
	}
	for _, choice := range r.Choices {
		if choice.Message == nil {
			continue
		}
		if strings.TrimSpace(choice.Message.Content) != "" || len(choice.Message.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// EmptyOutputReason explains why HasUsableAssistantOutput failed, in a form
// short enough to put in an error body. It returns "" when the response does
// have usable output, so callers can use it as the sole check.
//
// The distinctions matter operationally: a content filter, a reasoning-only
// reply and a truncated-before-any-text reply all look identical ("empty
// content") without this breakdown, but need completely different fixes.
func (r *ChatCompletionResponse) EmptyOutputReason() string {
	if r == nil {
		return "response body was empty"
	}
	if r.HasUsableAssistantOutput() {
		return ""
	}
	if len(r.Choices) == 0 {
		return "upstream returned zero choices"
	}

	reasons := make([]string, 0, len(r.Choices))
	for _, choice := range r.Choices {
		reasons = append(reasons, choice.emptyReason())
	}
	if len(reasons) == 1 {
		return reasons[0]
	}
	return fmt.Sprintf("all %d choices unusable: %s", len(reasons), strings.Join(reasons, "; "))
}

func (c ChatCompletionChoice) emptyReason() string {
	if c.Message == nil {
		return "choice carried no message"
	}
	detail := "empty content and no tool calls"
	if strings.TrimSpace(c.Message.ReasoningContent) != "" {
		detail = "only reasoning_content, no content or tool calls"
	}
	if c.FinishReason == nil {
		return detail
	}
	if reason := strings.TrimSpace(*c.FinishReason); reason != "" {
		return fmt.Sprintf("%s (finish_reason=%s)", detail, reason)
	}
	return detail
}

// EmptyOutputIsDeterministic reports whether an unusable response would come
// back unusable from any other provider serving the same request.
//
// The distinction drives failover: a truncated, filtered or reasoning-only
// reply is a property of the request or the model, so retrying it down the
// chain burns a second provider's quota to reproduce the same emptiness. A
// malformed reply (no choices, no message) is a misbehaving upstream, and the
// next provider genuinely may answer.
//
// It reports false when the response is usable, and requires *every* choice to
// be deterministically empty — one salvageable choice makes a retry worthwhile.
func (r *ChatCompletionResponse) EmptyOutputIsDeterministic() bool {
	if r == nil || r.HasUsableAssistantOutput() || len(r.Choices) == 0 {
		return false
	}
	for _, choice := range r.Choices {
		if !choice.emptyDeterministically() {
			return false
		}
	}
	return true
}

func (c ChatCompletionChoice) emptyDeterministically() bool {
	if c.Message == nil {
		return false
	}
	// A model that spent its budget thinking will do so again elsewhere.
	if strings.TrimSpace(c.Message.ReasoningContent) != "" {
		return true
	}
	if c.FinishReason == nil {
		return false
	}
	switch strings.TrimSpace(*c.FinishReason) {
	case "length":
		// max_tokens travels with the request, so every provider truncates.
		return true
	case "content_filter":
		return true
	default:
		// "stop" with empty content is sampling luck; a retry may well differ.
		return false
	}
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
