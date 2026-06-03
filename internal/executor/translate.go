package executor

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/user/cli-proxy/internal/types"
)

func ToAnthropicRequest(req *types.ChatCompletionRequest, model string) *types.AnthropicRequest {
	ar := &types.AnthropicRequest{
		Model:            model,
		MaxTokens:        req.MaxTokens,
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		Stream:           req.Stream,
		AnthropicVersion: "vertex-2023-10-16",
	}
	if ar.MaxTokens == 0 {
		ar.MaxTokens = 8192
	}

	var systemParts []string
	for _, msg := range req.Messages {
		if msg.Role == "system" || msg.Role == "developer" {
			systemParts = append(systemParts, extractText(msg.Content))
		}
	}
	if len(systemParts) > 0 {
		ar.System = strings.Join(systemParts, "\n\n")
	}

	for _, msg := range req.Messages {
		switch msg.Role {
		case "system", "developer":
			continue
		case "user":
			ar.Messages = append(ar.Messages, types.AnthropicMessage{
				Role:    "user",
				Content: msg.Content,
			})
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				blocks := buildAssistantBlocks(msg)
				raw, _ := json.Marshal(blocks)
				ar.Messages = append(ar.Messages, types.AnthropicMessage{
					Role:    "assistant",
					Content: raw,
				})
			} else {
				ar.Messages = append(ar.Messages, types.AnthropicMessage{
					Role:    "assistant",
					Content: msg.Content,
				})
			}
		case "tool":
			block := types.AnthropicContentBlock{
				Type:      "tool_result",
				ToolUseID: msg.ToolCallID,
				Content:   extractText(msg.Content),
			}
			raw, _ := json.Marshal([]types.AnthropicContentBlock{block})
			ar.Messages = append(ar.Messages, types.AnthropicMessage{
				Role:    "user",
				Content: raw,
			})
		}
	}

	for _, tool := range req.Tools {
		ar.Tools = append(ar.Tools, types.AnthropicTool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			InputSchema: tool.Function.Parameters,
		})
	}

	if stop := parseStop(req.Stop); len(stop) > 0 {
		ar.StopSequences = stop
	}

	return ar
}

func buildAssistantBlocks(msg types.ChatMessage) []types.AnthropicContentBlock {
	var blocks []types.AnthropicContentBlock
	text := extractText(msg.Content)
	if text != "" {
		blocks = append(blocks, types.AnthropicContentBlock{Type: "text", Text: text})
	}
	for _, tc := range msg.ToolCalls {
		blocks = append(blocks, types.AnthropicContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(tc.Function.Arguments),
		})
	}
	return blocks
}

func FromAnthropicResponse(resp *types.AnthropicResponse, model string) *types.ChatCompletionResponse {
	result := &types.ChatResult{Role: "assistant"}
	var toolCalls []types.ToolCall

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			result.Content += block.Text
		case "tool_use":
			args, _ := json.Marshal(block.Input)
			toolCalls = append(toolCalls, types.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: types.ToolCallFunction{
					Name:      block.Name,
					Arguments: string(args),
				},
			})
		}
	}
	if len(toolCalls) > 0 {
		result.ToolCalls = toolCalls
	}

	finishReason := mapStopReason(resp.StopReason)

	return &types.ChatCompletionResponse{
		ID:      fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:24]),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []types.ChatCompletionChoice{
			{Index: 0, Message: result, FinishReason: &finishReason},
		},
		Usage: &types.Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
}

func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var texts []string
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

func parseStop(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		return arr
	}
	return nil
}

func mapStopReason(reason string) string {
	switch reason {
	case "end_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	default:
		return "stop"
	}
}
