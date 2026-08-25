package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// compactSummaryPrefix marks summaries this proxy synthesized so a later
// request can decode them. Real OpenAI encrypted_content blobs will not match
// and are ignored.
const compactSummaryPrefix = "ken-compact-v1:"

const (
	compactMaxTokens = 1024
	compactPrompt    = "Summarize the conversation so far for continuity in a future context window. " +
		"Preserve goals, decisions, file paths, commands, open TODOs, and the next concrete steps. " +
		"Output only the summary."
)

func encodeCompactSummary(summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return ""
	}
	return base64.StdEncoding.EncodeToString([]byte(compactSummaryPrefix + summary))
}

func decodeCompactSummary(encrypted string) (string, bool) {
	encrypted = strings.TrimSpace(encrypted)
	if encrypted == "" {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return "", false
	}
	text := string(raw)
	if !strings.HasPrefix(text, compactSummaryPrefix) {
		return "", false
	}
	summary := strings.TrimSpace(strings.TrimPrefix(text, compactSummaryPrefix))
	if summary == "" {
		return "", false
	}
	return summary, true
}

func trailingCompactionTrigger(input json.RawMessage) bool {
	var items []responsesInputItem
	if err := json.Unmarshal(input, &items); err != nil || len(items) == 0 {
		return false
	}
	return strings.EqualFold(items[len(items)-1].Type, "compaction_trigger")
}

func prepareCompactionRequest(req *types.ChatCompletionRequest) *types.ChatCompletionRequest {
	compact := *req
	compact.Stream = false
	compact.Tools = nil
	compact.ToolChoice = nil
	compact.ReasoningEffort = ""
	if compact.MaxTokens <= 0 || compact.MaxTokens > compactMaxTokens {
		compact.MaxTokens = compactMaxTokens
	}
	content, _ := json.Marshal(compactPrompt)
	compact.Messages = append(append([]types.ChatMessage(nil), req.Messages...), types.ChatMessage{
		Role:    "user",
		Content: content,
	})
	return &compact
}

func chatResponseSummary(resp *types.ChatCompletionResponse) string {
	if resp == nil {
		return ""
	}
	for _, choice := range resp.Choices {
		if choice.Message == nil {
			continue
		}
		if text := strings.TrimSpace(choice.Message.Content); text != "" {
			return text
		}
		if text := strings.TrimSpace(choice.Message.ReasoningContent); text != "" {
			return text
		}
	}
	return ""
}

func (h *ResponsesHandler) handleCompactionV2(
	c *gin.Context,
	ctx context.Context,
	exec executor.Executor,
	chatReq *types.ChatCompletionRequest,
	model string,
	start time.Time,
	getAccount func() (string, []string),
) {
	compactReq := prepareCompactionRequest(chatReq)
	resp, execErr := exec.Execute(ctx, compactReq)
	account, failedOver := getAccount()
	if execErr != nil {
		log.Printf("responses compaction error: %v", execErr)
		h.recordLog(c, model, start, nil, account, failedOver, execErr)
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{"message": execErr.Error(), "type": "server_error"},
		})
		return
	}
	summary := chatResponseSummary(resp)
	if summary == "" {
		emptyErr := fmt.Errorf("compaction produced no summary")
		log.Printf("responses compaction error: %v", emptyErr)
		h.recordLog(c, model, start, nil, account, failedOver, emptyErr)
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{"message": emptyErr.Error(), "type": "server_error"},
		})
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Writer.Flush()

	writeErr := writeCompactionV2SSE(resp, model, summary, c.Writer)
	var breakdown *types.TokenUsage
	if resp != nil && resp.Usage != nil {
		b := resp.Usage.Breakdown()
		breakdown = &b
	}
	h.recordLog(c, model, start, breakdown, account, failedOver, writeErr)
	if writeErr != nil {
		log.Printf("responses compaction write error: %v", writeErr)
	}
}

func writeCompactionV2SSE(resp *types.ChatCompletionResponse, requestedModel, summary string, w io.Writer) error {
	flusher, canFlush := w.(interface{ Flush() })
	writeEvent := func(eventType string, data interface{}) error {
		payload, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("marshal %s event: %w", eventType, err)
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, payload); err != nil {
			return err
		}
		if canFlush {
			flusher.Flush()
		}
		return nil
	}

	model := requestedModel
	if resp != nil && resp.Model != "" {
		model = resp.Model
	}
	responseID := fmt.Sprintf("resp_%s", uuid.New().String()[:29])
	itemID := fmt.Sprintf("cmp_%s", uuid.New().String()[:28])
	createdAt := time.Now().Unix()
	item := map[string]interface{}{
		"id":                itemID,
		"type":              "compaction",
		"encrypted_content": encodeCompactSummary(summary),
	}
	if err := writeEvent("response.created", map[string]interface{}{
		"type": "response.created",
		"response": map[string]interface{}{
			"id": responseID, "object": "response", "status": "in_progress",
			"created_at": createdAt, "model": model, "output": []interface{}{},
		},
	}); err != nil {
		return err
	}
	if err := writeEvent("response.output_item.done", map[string]interface{}{
		"type": "response.output_item.done", "response_id": responseID,
		"output_index": 0, "item": item,
	}); err != nil {
		return err
	}

	completedResponse := map[string]interface{}{
		"id": responseID, "object": "response", "status": "completed",
		"created_at": createdAt, "model": model, "output": []interface{}{item},
	}
	if resp != nil && resp.Usage != nil {
		completedResponse["usage"] = chatUsageAsResponses(resp.Usage)
	}
	return writeEvent("response.completed", map[string]interface{}{
		"type": "response.completed", "response": completedResponse,
	})
}
