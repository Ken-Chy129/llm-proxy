package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
	"github.com/Ken-Chy129/llm-proxy/internal/stats"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

type ResponsesHandler struct {
	router  *router.Router
	statsDB *stats.DB
}

func NewResponsesHandler(r *router.Router, db *stats.DB) *ResponsesHandler {
	return &ResponsesHandler{router: r, statsDB: db}
}

type responsesRequest struct {
	Model        string            `json:"model"`
	Instructions string            `json:"instructions,omitempty"`
	Input        json.RawMessage   `json:"input"`
	Stream       *bool             `json:"stream,omitempty"`
	Tools        []json.RawMessage `json:"tools,omitempty"`
	ToolChoice   json.RawMessage   `json:"tool_choice,omitempty"`
	Reasoning    *struct {
		Effort  string `json:"effort,omitempty"`
		Summary string `json:"summary,omitempty"`
	} `json:"reasoning,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
}

type responsesInputItem struct {
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Type      string          `json:"type,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    string          `json:"output,omitempty"`
}

func (h *ResponsesHandler) HandleResponses(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "failed to read body", "type": "invalid_request_error"},
		})
		return
	}

	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "invalid request: " + err.Error(), "type": "invalid_request_error"},
		})
		return
	}

	exec, err := h.router.Resolve(req.Model)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{"message": err.Error(), "type": "invalid_request_error", "code": "model_not_found"},
		})
		return
	}

	start := time.Now()

	setSSEHeaders := func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
	}

	if re, ok := exec.(executor.ResponsesExecutor); ok {
		stream, openErr := re.OpenResponsesStream(c.Request.Context(), body)
		if openErr != nil {
			log.Printf("responses open error: %v", openErr)
			h.recordLog(req.Model, start, openErr)
			c.JSON(http.StatusBadGateway, gin.H{
				"error": gin.H{"message": openErr.Error(), "type": "server_error"},
			})
			return
		}
		defer stream.Close()

		setSSEHeaders()
		c.Writer.Flush()
		copyErr := copyWithFlush(stream, c.Writer)
		h.recordLog(req.Model, start, copyErr)
		if copyErr != nil {
			log.Printf("responses stream error: %v", copyErr)
		}
		return
	}

	chatReq := h.toChatCompletionRequest(&req)
	setSSEHeaders()
	c.Writer.Flush()
	streamErr := h.streamWithTranslation(c.Request.Context(), exec, chatReq, c.Writer)
	h.recordLog(req.Model, start, streamErr)
	if streamErr != nil {
		log.Printf("responses translate stream error: %v", streamErr)
	}
}

func (h *ResponsesHandler) toChatCompletionRequest(req *responsesRequest) *types.ChatCompletionRequest {
	chatReq := &types.ChatCompletionRequest{
		Model:       req.Model,
		Stream:      true,
		MaxTokens:   req.MaxOutputTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}

	if req.Instructions != "" {
		sysContent, _ := json.Marshal(req.Instructions)
		chatReq.Messages = append(chatReq.Messages, types.ChatMessage{
			Role:    "system",
			Content: sysContent,
		})
	}

	var inputItems []responsesInputItem
	json.Unmarshal(req.Input, &inputItems)

	for _, item := range inputItems {
		switch {
		case item.Role == "user" || item.Role == "assistant":
			content := item.Content
			if len(content) == 0 {
				content, _ = json.Marshal("")
			}
			msg := types.ChatMessage{Role: item.Role, Content: content}
			chatReq.Messages = append(chatReq.Messages, msg)
		case item.Type == "function_call":
			chatReq.Messages = append(chatReq.Messages, types.ChatMessage{
				Role: "assistant",
				ToolCalls: []types.ToolCall{{
					ID:   item.CallID,
					Type: "function",
					Function: types.ToolCallFunction{
						Name:      item.Name,
						Arguments: item.Arguments,
					},
				}},
			})
		case item.Type == "function_call_output":
			outputContent, _ := json.Marshal(item.Output)
			chatReq.Messages = append(chatReq.Messages, types.ChatMessage{
				Role:       "tool",
				Content:    outputContent,
				ToolCallID: item.CallID,
			})
		}
	}

	if len(chatReq.Messages) == 0 {
		emptyContent, _ := json.Marshal("")
		chatReq.Messages = append(chatReq.Messages, types.ChatMessage{
			Role:    "user",
			Content: emptyContent,
		})
	}

	for _, tool := range req.Tools {
		var t types.Tool
		if json.Unmarshal(tool, &t) == nil {
			chatReq.Tools = append(chatReq.Tools, t)
		}
	}
	chatReq.ToolChoice = req.ToolChoice

	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		chatReq.ReasoningEffort = req.Reasoning.Effort
	}

	return chatReq
}

func (h *ResponsesHandler) streamWithTranslation(ctx context.Context, exec executor.Executor, req *types.ChatCompletionRequest, w io.Writer) error {
	var buf bytes.Buffer
	if _, err := exec.ExecuteStream(ctx, req, &buf); err != nil {
		return err
	}

	flusher, canFlush := w.(interface{ Flush() })

	responseID := fmt.Sprintf("resp_%s", uuid.New().String()[:29])
	outputItemID := fmt.Sprintf("item_%s", uuid.New().String()[:29])
	contentPartIdx := 0

	writeEvent := func(eventType string, data interface{}) {
		j, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(j))
		if canFlush {
			flusher.Flush()
		}
	}

	writeEvent("response.created", map[string]interface{}{
		"type": "response.created",
		"response": map[string]interface{}{
			"id":     responseID,
			"object": "response",
			"status": "in_progress",
			"model":  req.Model,
			"output": []interface{}{},
		},
	})

	writeEvent("response.in_progress", map[string]interface{}{
		"type": "response.in_progress",
		"response": map[string]interface{}{
			"id":     responseID,
			"object": "response",
			"status": "in_progress",
		},
	})

	outputItemSent := false
	var fullContent strings.Builder
	var toolCalls []map[string]interface{}

	scanner := bufio.NewScanner(&buf)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk types.ChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		for _, choice := range chunk.Choices {
			delta := choice.Delta
			if delta == nil {
				continue
			}

			if delta.Content != "" {
				if !outputItemSent {
					writeEvent("response.output_item.added", map[string]interface{}{
						"type":         "response.output_item.added",
						"output_index": 0,
						"item": map[string]interface{}{
							"id":      outputItemID,
							"type":    "message",
							"role":    "assistant",
							"status":  "in_progress",
							"content": []interface{}{},
						},
					})
					writeEvent("response.content_part.added", map[string]interface{}{
						"type":         "response.content_part.added",
						"item_id":      outputItemID,
						"output_index": 0,
						"content_index": contentPartIdx,
						"part": map[string]interface{}{
							"type": "output_text",
							"text": "",
						},
					})
					outputItemSent = true
				}

				fullContent.WriteString(delta.Content)
				writeEvent("response.output_text.delta", map[string]interface{}{
					"type":          "response.output_text.delta",
					"item_id":       outputItemID,
					"output_index":  0,
					"content_index": contentPartIdx,
					"delta":         delta.Content,
				})
			}

			if len(delta.ToolCalls) > 0 {
				for _, tc := range delta.ToolCalls {
					if tc.ID != "" {
						tcItemID := fmt.Sprintf("fc_%s", uuid.New().String()[:30])
						writeEvent("response.output_item.added", map[string]interface{}{
							"type":         "response.output_item.added",
							"output_index": len(toolCalls) + 1,
							"item": map[string]interface{}{
								"id":        tcItemID,
								"type":      "function_call",
								"status":    "in_progress",
								"call_id":   tc.ID,
								"name":      tc.Function.Name,
								"arguments": "",
							},
						})
						toolCalls = append(toolCalls, map[string]interface{}{
							"item_id": tcItemID,
							"call_id": tc.ID,
							"name":    tc.Function.Name,
							"index":   len(toolCalls) + 1,
						})
					}
					if tc.Function.Arguments != "" && len(toolCalls) > 0 {
						last := toolCalls[len(toolCalls)-1]
						writeEvent("response.function_call_arguments.delta", map[string]interface{}{
							"type":         "response.function_call_arguments.delta",
							"item_id":      last["item_id"],
							"output_index": last["index"],
							"delta":        tc.Function.Arguments,
						})
					}
				}
			}

			if choice.FinishReason != nil {
				if outputItemSent && fullContent.Len() > 0 {
					writeEvent("response.output_text.done", map[string]interface{}{
						"type":          "response.output_text.done",
						"item_id":       outputItemID,
						"output_index":  0,
						"content_index": contentPartIdx,
						"text":          fullContent.String(),
					})
					writeEvent("response.content_part.done", map[string]interface{}{
						"type":          "response.content_part.done",
						"item_id":       outputItemID,
						"output_index":  0,
						"content_index": contentPartIdx,
						"part": map[string]interface{}{
							"type": "output_text",
							"text": fullContent.String(),
						},
					})
					writeEvent("response.output_item.done", map[string]interface{}{
						"type":         "response.output_item.done",
						"output_index": 0,
						"item": map[string]interface{}{
							"id":     outputItemID,
							"type":   "message",
							"role":   "assistant",
							"status": "completed",
							"content": []interface{}{
								map[string]interface{}{
									"type": "output_text",
									"text": fullContent.String(),
								},
							},
						},
					})
				}

				for _, tc := range toolCalls {
					writeEvent("response.output_item.done", map[string]interface{}{
						"type":         "response.output_item.done",
						"output_index": tc["index"],
						"item": map[string]interface{}{
							"id":     tc["item_id"],
							"type":   "function_call",
							"status": "completed",
						},
					})
				}

				writeEvent("response.completed", map[string]interface{}{
					"type": "response.completed",
					"response": map[string]interface{}{
						"id":     responseID,
						"object": "response",
						"status": "completed",
						"model":  req.Model,
					},
				})
			}
		}
	}

	return nil
}

func copyWithFlush(src io.Reader, dst io.Writer) error {
	flusher, canFlush := dst.(interface{ Flush() })
	buf := make([]byte, 4096)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, wErr := dst.Write(buf[:n]); wErr != nil {
				return wErr
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (h *ResponsesHandler) recordLog(model string, start time.Time, err error) {
	if h.statsDB == nil {
		return
	}
	entry := &stats.RequestLog{
		Time:      time.Now(),
		Model:     model,
		Backend:   h.router.BackendName(model),
		LatencyMs: time.Since(start).Milliseconds(),
		Stream:    true,
		Status:    http.StatusOK,
	}
	if err != nil {
		entry.Status = errStatus(err)
		entry.Error = err.Error()
	}
	if recordErr := h.statsDB.Record(entry); recordErr != nil {
		log.Printf("stats record error: %v", recordErr)
	}
}
