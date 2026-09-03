package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
	"github.com/Ken-Chy129/llm-proxy/internal/stats"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type ResponsesHandler struct {
	router  *router.Router
	statsDB *stats.DB
}

func NewResponsesHandler(r *router.Router, db *stats.DB) *ResponsesHandler {
	return &ResponsesHandler{router: r, statsDB: db}
}

// servingBackend reports which provider handled the request: the one the chain
// recorded, falling back to the head of the model's chain when nothing ran.
func (h *ResponsesHandler) servingBackend(c *gin.Context, model string) string {
	if served := executor.ServingBackend(c.Request.Context()); served != "" {
		return served
	}
	return h.router.BackendName(model)
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
	Role             string          `json:"role,omitempty"`
	Content          json.RawMessage `json:"content,omitempty"`
	Type             string          `json:"type,omitempty"`
	CallID           string          `json:"call_id,omitempty"`
	Name             string          `json:"name,omitempty"`
	Arguments        json.RawMessage `json:"arguments,omitempty"`
	Output           json.RawMessage `json:"output,omitempty"`
	EncryptedContent string          `json:"encrypted_content,omitempty"`
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

	ctx, getAccount := executor.WithAccountRecorder(c.Request.Context())
	ctx, _ = executor.WithBackendRecorder(ctx)
	// Put the derived context back on the request so recordLog, which is called
	// from a dozen places with only the gin context, can read which provider
	// ended up serving.
	c.Request = c.Request.WithContext(ctx)

	if chain, ok := exec.(*executor.Chain); ok && chain.HasMixedResponsesSupport() {
		var chatReq *types.ChatCompletionRequest
		var conversionErr error
		var adaptedUsage *types.Usage
		stream, openErr := chain.OpenResponsesStreamWithAdapter(ctx, body, func(ctx context.Context, provider executor.Executor) (io.ReadCloser, error) {
			if chatReq == nil && conversionErr == nil {
				chatReq, conversionErr = h.toChatCompletionRequest(&req)
			}
			if conversionErr != nil {
				return nil, &executor.HTTPError{
					Backend: "responses adapter",
					Status:  http.StatusBadRequest,
					Body:    conversionErr.Error(),
				}
			}
			stream, usage, err := h.openAdaptedResponsesStream(ctx, provider, chatReq, req.Model, trailingCompactionTrigger(req.Input))
			if err == nil {
				adaptedUsage = usage
			}
			return stream, err
		})
		if openErr != nil {
			log.Printf("responses open error: %v", openErr)
			account, failedOver := getAccount()
			h.recordLog(c, req.Model, start, nil, account, failedOver, openErr)
			status := http.StatusBadGateway
			errorType := "server_error"
			var httpErr *executor.HTTPError
			if errors.As(openErr, &httpErr) && httpErr.Backend == "responses adapter" && httpErr.Status == http.StatusBadRequest {
				status = http.StatusBadRequest
				errorType = "invalid_request_error"
			}
			c.JSON(status, gin.H{
				"error": gin.H{"message": openErr.Error(), "type": errorType},
			})
			return
		}
		defer stream.Close()

		setSSEHeaders()
		c.Writer.Flush()
		responsesUsage, copyErr := copyResponsesStreamAndExtractUsage(stream, c.Writer)
		account, failedOver := getAccount()
		var breakdown *types.TokenUsage
		if responsesUsage != nil {
			b := responsesUsage.Breakdown()
			breakdown = &b
		} else if adaptedUsage != nil {
			b := adaptedUsage.Breakdown()
			breakdown = &b
		}
		h.recordLog(c, req.Model, start, breakdown, account, failedOver, copyErr)
		if copyErr != nil {
			log.Printf("responses stream error: %v", copyErr)
		}
		return
	}

	if re, ok := executor.AsResponsesExecutor(exec); ok {
		stream, openErr := re.OpenResponsesStream(ctx, body)
		if openErr != nil {
			log.Printf("responses open error: %v", openErr)
			account, failedOver := getAccount()
			h.recordLog(c, req.Model, start, nil, account, failedOver, openErr)
			c.JSON(http.StatusBadGateway, gin.H{
				"error": gin.H{"message": openErr.Error(), "type": "server_error"},
			})
			return
		}
		defer stream.Close()

		setSSEHeaders()
		c.Writer.Flush()
		usage, copyErr := copyResponsesStreamAndExtractUsage(stream, c.Writer)
		account, failedOver := getAccount()
		var breakdown *types.TokenUsage
		if usage != nil {
			b := usage.Breakdown()
			breakdown = &b
		}
		h.recordLog(c, req.Model, start, breakdown, account, failedOver, copyErr)
		if copyErr != nil {
			log.Printf("responses stream error: %v", copyErr)
		}
		return
	}

	chatReq, conversionErr := h.toChatCompletionRequest(&req)
	if conversionErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": conversionErr.Error(), "type": "invalid_request_error", "param": "input"},
		})
		return
	}
	if trailingCompactionTrigger(req.Input) {
		h.handleCompactionV2(c, ctx, exec, chatReq, req.Model, start, getAccount)
		return
	}
	if support, ok := exec.(executor.StreamingSupport); ok && !support.SupportsStreaming() {
		chatReq.Stream = false
		resp, execErr := exec.Execute(ctx, chatReq)
		account, failedOver := getAccount()
		if execErr != nil {
			log.Printf("responses non-streaming adapter error: %v", execErr)
			h.recordLog(c, req.Model, start, nil, account, failedOver, execErr)
			c.JSON(http.StatusBadGateway, gin.H{
				"error": gin.H{"message": execErr.Error(), "type": "server_error"},
			})
			return
		}
		if resp == nil {
			emptyErr := fmt.Errorf("non-streaming executor returned an empty response")
			log.Printf("responses non-streaming adapter error: %v", emptyErr)
			h.recordLog(c, req.Model, start, nil, account, failedOver, emptyErr)
			c.JSON(http.StatusBadGateway, gin.H{
				"error": gin.H{"message": emptyErr.Error(), "type": "server_error"},
			})
			return
		}

		setSSEHeaders()
		c.Writer.Flush()
		writeErr := writeChatCompletionAsResponsesSSE(resp, req.Model, c.Writer)
		var breakdown *types.TokenUsage
		if resp.Usage != nil {
			b := resp.Usage.Breakdown()
			breakdown = &b
		}
		h.recordLog(c, req.Model, start, breakdown, account, failedOver, writeErr)
		if writeErr != nil {
			log.Printf("responses non-streaming adapter write error: %v", writeErr)
		}
		return
	}

	setSSEHeaders()
	c.Writer.Flush()
	usage, streamErr := h.streamWithTranslation(ctx, exec, chatReq, c.Writer)
	account, failedOver := getAccount()
	var breakdown *types.TokenUsage
	if usage != nil {
		b := usage.Breakdown()
		breakdown = &b
	}
	h.recordLog(c, req.Model, start, breakdown, account, failedOver, streamErr)
	if streamErr != nil {
		log.Printf("responses translate stream error: %v", streamErr)
	}
}

// openAdaptedResponsesStream serves one non-native Responses provider and
// buffers the translated event stream before returning it. Buffering is what
// keeps provider failover safe: an adapted provider can fail without having
// written a partial Responses stream to the client.
func (h *ResponsesHandler) openAdaptedResponsesStream(
	ctx context.Context,
	exec executor.Executor,
	req *types.ChatCompletionRequest,
	model string,
	compaction bool,
) (io.ReadCloser, *types.Usage, error) {
	var buf bytes.Buffer
	if compaction {
		resp, err := exec.Execute(ctx, prepareCompactionRequest(req))
		if err != nil {
			return nil, nil, err
		}
		summary := chatResponseSummary(resp)
		if summary == "" {
			return nil, nil, fmt.Errorf("compaction produced no summary")
		}
		if err := writeCompactionV2SSE(resp, model, summary, &buf); err != nil {
			return nil, nil, err
		}
		var usage *types.Usage
		if resp != nil {
			usage = resp.Usage
		}
		return io.NopCloser(bytes.NewReader(buf.Bytes())), usage, nil
	}

	if support, ok := exec.(executor.StreamingSupport); ok && !support.SupportsStreaming() {
		nonStreaming := *req
		nonStreaming.Stream = false
		resp, err := exec.Execute(ctx, &nonStreaming)
		if err != nil {
			return nil, nil, err
		}
		if err := writeChatCompletionAsResponsesSSE(resp, model, &buf); err != nil {
			return nil, nil, err
		}
		return io.NopCloser(bytes.NewReader(buf.Bytes())), resp.Usage, nil
	}

	usage, err := h.streamWithTranslation(ctx, exec, req, &buf)
	if err != nil {
		return nil, usage, err
	}
	return io.NopCloser(bytes.NewReader(buf.Bytes())), usage, nil
}

func (h *ResponsesHandler) toChatCompletionRequest(req *responsesRequest) (*types.ChatCompletionRequest, error) {
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
	if err := json.Unmarshal(req.Input, &inputItems); err != nil {
		return nil, fmt.Errorf("invalid Responses input: %w", err)
	}

	var pendingToolCalls []types.ToolCall
	unresolvedToolCalls := make(map[string]struct{})
	flushToolCalls := func() error {
		if len(pendingToolCalls) == 0 {
			return nil
		}
		for _, toolCall := range pendingToolCalls {
			if _, duplicate := unresolvedToolCalls[toolCall.ID]; duplicate {
				return fmt.Errorf("duplicate function call %q", toolCall.ID)
			}
			unresolvedToolCalls[toolCall.ID] = struct{}{}
		}
		chatReq.Messages = append(chatReq.Messages, types.ChatMessage{
			Role:      "assistant",
			ToolCalls: pendingToolCalls,
		})
		pendingToolCalls = nil
		return nil
	}
	firstUnresolvedCall := func() string {
		for callID := range unresolvedToolCalls {
			return callID
		}
		return ""
	}

	for _, item := range inputItems {
		switch {
		case strings.EqualFold(item.Type, "compaction_trigger"):
			// Codex remote-compaction-v2 marker. Handled by handleCompactionV2;
			// Completions upstreams must never see it.
			continue
		case strings.EqualFold(item.Type, "compaction"):
			if err := flushToolCalls(); err != nil {
				return nil, err
			}
			if callID := firstUnresolvedCall(); callID != "" {
				return nil, fmt.Errorf("no function_call_output found for function call %q before the next message", callID)
			}
			summary, ok := decodeCompactSummary(item.EncryptedContent)
			if !ok {
				continue
			}
			content, err := json.Marshal("[Conversation summary after compaction]\n" + summary)
			if err != nil {
				return nil, fmt.Errorf("encode compaction summary: %w", err)
			}
			chatReq.Messages = append(chatReq.Messages, types.ChatMessage{
				Role:    "user",
				Content: content,
			})
		case item.Type == "function_call":
			if len(unresolvedToolCalls) > 0 {
				return nil, fmt.Errorf("no function_call_output found for function call %q before the next call", firstUnresolvedCall())
			}
			if item.CallID == "" || item.Name == "" {
				return nil, fmt.Errorf("function_call requires call_id and name")
			}
			arguments, err := chatToolCallArguments(item.Arguments)
			if err != nil {
				return nil, fmt.Errorf("invalid arguments for function call %q: %w", item.CallID, err)
			}
			pendingToolCalls = append(pendingToolCalls, types.ToolCall{
				ID:   item.CallID,
				Type: "function",
				Function: types.ToolCallFunction{
					Name:      item.Name,
					Arguments: arguments,
				},
			})
		case item.Type == "function_call_output":
			if err := flushToolCalls(); err != nil {
				return nil, err
			}
			if item.CallID == "" {
				return nil, fmt.Errorf("function_call_output requires call_id")
			}
			if _, ok := unresolvedToolCalls[item.CallID]; !ok {
				return nil, fmt.Errorf("function_call_output %q has no matching function call", item.CallID)
			}
			outputContent, err := chatToolOutputContent(item.Output)
			if err != nil {
				return nil, fmt.Errorf("invalid output for function call %q: %w", item.CallID, err)
			}
			chatReq.Messages = append(chatReq.Messages, types.ChatMessage{
				Role:       "tool",
				Content:    outputContent,
				ToolCallID: item.CallID,
			})
			delete(unresolvedToolCalls, item.CallID)
		case item.Role == "user" || item.Role == "assistant":
			if err := flushToolCalls(); err != nil {
				return nil, err
			}
			if callID := firstUnresolvedCall(); callID != "" {
				return nil, fmt.Errorf("no function_call_output found for function call %q before the next message", callID)
			}
			content := normalizeResponsesContent(item.Content)
			if len(content) == 0 {
				content, _ = json.Marshal("")
			}
			msg := types.ChatMessage{Role: item.Role, Content: content}
			chatReq.Messages = append(chatReq.Messages, msg)
		}
	}
	if err := flushToolCalls(); err != nil {
		return nil, err
	}
	if callID := firstUnresolvedCall(); callID != "" {
		return nil, fmt.Errorf("no function_call_output found for function call %q", callID)
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
		if json.Unmarshal(tool, &t) == nil && t.Function.Name != "" {
			chatReq.Tools = append(chatReq.Tools, t)
			continue
		}
		var responseTool struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters,omitempty"`
		}
		if json.Unmarshal(tool, &responseTool) == nil && responseTool.Type == "function" && responseTool.Name != "" {
			chatReq.Tools = append(chatReq.Tools, types.Tool{
				Type: "function",
				Function: types.ToolFunction{
					Name:        responseTool.Name,
					Description: responseTool.Description,
					Parameters:  responseTool.Parameters,
				},
			})
		}
	}
	chatReq.ToolChoice = req.ToolChoice

	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		chatReq.ReasoningEffort = req.Reasoning.Effort
	}

	return chatReq, nil
}

func chatToolCallArguments(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", nil
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return "", fmt.Errorf("must not be null")
	}

	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return text, nil
	}

	if trimmed[0] != '{' && trimmed[0] != '[' {
		return "", fmt.Errorf("must be a JSON string, object, or array")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err != nil {
		return "", err
	}
	return compact.String(), nil
}

func chatToolOutputContent(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, fmt.Errorf("output is required")
	}

	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		if text == "" {
			text = "(tool returned no output)"
		}
		return json.Marshal(text)
	}

	var items []map[string]interface{}
	if err := json.Unmarshal(trimmed, &items); err == nil {
		parts := make([]string, 0, len(items))
		for _, item := range items {
			if itemText, _ := item["text"].(string); itemText != "" {
				parts = append(parts, itemText)
				continue
			}
			switch itemType, _ := item["type"].(string); itemType {
			case "input_image", "image", "image_url":
				parts = append(parts, "[tool returned an image; image content is unavailable through the Chat Completions adapter]")
			case "encrypted_content":
				parts = append(parts, "[tool returned encrypted content]")
			default:
				encoded, err := json.Marshal(item)
				if err != nil {
					return nil, err
				}
				parts = append(parts, string(encoded))
			}
		}
		if len(parts) == 0 {
			parts = append(parts, "(tool returned no output)")
		}
		return json.Marshal(strings.Join(parts, "\n"))
	}

	var value interface{}
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(string(encoded))
}

// normalizeResponsesContent converts Responses API content block names into
// their Chat Completions equivalents. Codex sends input_text/input_image while
// OpenAI-compatible upstreams such as Kimi expect text/image_url.
func normalizeResponsesContent(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var blocks []map[string]interface{}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return raw
	}
	for _, block := range blocks {
		typeName, _ := block["type"].(string)
		switch typeName {
		case "input_text", "output_text":
			block["type"] = "text"
		case "input_image":
			block["type"] = "image_url"
			if imageURL, ok := block["image_url"]; ok {
				if _, isObject := imageURL.(map[string]interface{}); !isObject {
					block["image_url"] = map[string]interface{}{"url": imageURL}
				}
			}
		}
	}
	normalized, err := json.Marshal(blocks)
	if err != nil {
		return raw
	}
	return normalized
}

// streamWithTranslation runs a chat-shaped executor and re-emits its output as
// Responses API events. It returns the executor's usage so the caller can log it
// — this used to be discarded, which is why translated /v1/responses traffic
// logged zero tokens.
func (h *ResponsesHandler) streamWithTranslation(ctx context.Context, exec executor.Executor, req *types.ChatCompletionRequest, w io.Writer) (*types.Usage, error) {
	var buf bytes.Buffer
	usage, err := exec.ExecuteStream(ctx, req, &buf)
	if err != nil {
		return usage, err
	}

	flusher, canFlush := w.(interface{ Flush() })

	responseID := fmt.Sprintf("resp_%s", uuid.New().String()[:29])
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

	type streamedToolCall struct {
		itemID      string
		callID      string
		name        string
		outputIndex int
		arguments   strings.Builder
	}

	outputItemSent := false
	outputItemID := ""
	textOutputIndex := -1
	nextOutputIndex := 0
	responseCompleted := false
	var fullContent strings.Builder
	var toolCalls []*streamedToolCall

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
					outputItemID = fmt.Sprintf("item_%s", uuid.New().String()[:29])
					textOutputIndex = nextOutputIndex
					nextOutputIndex++
					writeEvent("response.output_item.added", map[string]interface{}{
						"type":         "response.output_item.added",
						"response_id":  responseID,
						"output_index": textOutputIndex,
						"item": map[string]interface{}{
							"id":      outputItemID,
							"type":    "message",
							"role":    "assistant",
							"status":  "in_progress",
							"content": []interface{}{},
						},
					})
					writeEvent("response.content_part.added", map[string]interface{}{
						"type":          "response.content_part.added",
						"response_id":   responseID,
						"item_id":       outputItemID,
						"output_index":  textOutputIndex,
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
					"response_id":   responseID,
					"item_id":       outputItemID,
					"output_index":  textOutputIndex,
					"content_index": contentPartIdx,
					"delta":         delta.Content,
				})
			}

			if len(delta.ToolCalls) > 0 {
				for _, tc := range delta.ToolCalls {
					var current *streamedToolCall
					if tc.ID != "" {
						current = &streamedToolCall{
							itemID:      fmt.Sprintf("fc_%s", uuid.New().String()[:30]),
							callID:      tc.ID,
							name:        tc.Function.Name,
							outputIndex: nextOutputIndex,
						}
						nextOutputIndex++
						writeEvent("response.output_item.added", map[string]interface{}{
							"type":         "response.output_item.added",
							"response_id":  responseID,
							"output_index": current.outputIndex,
							"item": map[string]interface{}{
								"id":        current.itemID,
								"type":      "function_call",
								"status":    "in_progress",
								"call_id":   current.callID,
								"name":      current.name,
								"arguments": "",
							},
						})
						toolCalls = append(toolCalls, current)
					} else if len(toolCalls) > 0 {
						current = toolCalls[len(toolCalls)-1]
					}
					if tc.Function.Arguments != "" && current != nil {
						current.arguments.WriteString(tc.Function.Arguments)
						writeEvent("response.function_call_arguments.delta", map[string]interface{}{
							"type":         "response.function_call_arguments.delta",
							"response_id":  responseID,
							"item_id":      current.itemID,
							"output_index": current.outputIndex,
							"delta":        tc.Function.Arguments,
						})
					}
				}
			}

			if choice.FinishReason != nil && !responseCompleted {
				responseCompleted = true
				completedOutput := make([]interface{}, nextOutputIndex)
				if outputItemSent && fullContent.Len() > 0 {
					writeEvent("response.output_text.done", map[string]interface{}{
						"type":          "response.output_text.done",
						"response_id":   responseID,
						"item_id":       outputItemID,
						"output_index":  textOutputIndex,
						"content_index": contentPartIdx,
						"text":          fullContent.String(),
					})
					writeEvent("response.content_part.done", map[string]interface{}{
						"type":          "response.content_part.done",
						"response_id":   responseID,
						"item_id":       outputItemID,
						"output_index":  textOutputIndex,
						"content_index": contentPartIdx,
						"part": map[string]interface{}{
							"type": "output_text",
							"text": fullContent.String(),
						},
					})
					completedItem := map[string]interface{}{
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
					}
					writeEvent("response.output_item.done", map[string]interface{}{
						"type":         "response.output_item.done",
						"response_id":  responseID,
						"output_index": textOutputIndex,
						"item":         completedItem,
					})
					completedOutput[textOutputIndex] = completedItem
				}

				for _, tc := range toolCalls {
					arguments := tc.arguments.String()
					writeEvent("response.function_call_arguments.done", map[string]interface{}{
						"type":         "response.function_call_arguments.done",
						"response_id":  responseID,
						"item_id":      tc.itemID,
						"output_index": tc.outputIndex,
						"call_id":      tc.callID,
						"name":         tc.name,
						"arguments":    arguments,
					})
					completedItem := map[string]interface{}{
						"id":        tc.itemID,
						"type":      "function_call",
						"status":    "completed",
						"call_id":   tc.callID,
						"name":      tc.name,
						"arguments": arguments,
					}
					writeEvent("response.output_item.done", map[string]interface{}{
						"type":         "response.output_item.done",
						"response_id":  responseID,
						"output_index": tc.outputIndex,
						"item":         completedItem,
					})
					completedOutput[tc.outputIndex] = completedItem
				}

				completedResponse := map[string]interface{}{
					"id":     responseID,
					"object": "response",
					"status": "completed",
					"model":  req.Model,
					"output": completedOutput,
				}
				if usage != nil {
					completedResponse["usage"] = chatUsageAsResponses(usage)
				}
				writeEvent("response.completed", map[string]interface{}{
					"type":     "response.completed",
					"response": completedResponse,
				})
			}
		}
	}

	return usage, nil
}

// writeChatCompletionAsResponsesSSE adapts a completed Chat Completions result
// into the Responses API event sequence required by clients such as Codex CLI.
// The upstream call is still non-streaming: once it completes, the proxy emits
// one text delta (or one arguments delta per tool call) and then the matching
// done/completed events.
func writeChatCompletionAsResponsesSSE(resp *types.ChatCompletionResponse, requestedModel string, w io.Writer) error {
	if resp == nil {
		return fmt.Errorf("non-streaming executor returned an empty response")
	}

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

	model := resp.Model
	if model == "" {
		model = requestedModel
	}
	responseID := fmt.Sprintf("resp_%s", uuid.New().String()[:29])
	createdAt := time.Now().Unix()
	if err := writeEvent("response.created", map[string]interface{}{
		"type": "response.created",
		"response": map[string]interface{}{
			"id": responseID, "object": "response", "status": "in_progress",
			"created_at": createdAt, "model": model, "output": []interface{}{},
		},
	}); err != nil {
		return err
	}
	if err := writeEvent("response.in_progress", map[string]interface{}{
		"type": "response.in_progress",
		"response": map[string]interface{}{
			"id": responseID, "object": "response", "status": "in_progress",
			"created_at": createdAt, "model": model, "output": []interface{}{},
		},
	}); err != nil {
		return err
	}

	outputIndex := 0
	completedOutput := []interface{}{}
	for _, choice := range resp.Choices {
		if choice.Message == nil {
			continue
		}
		message := choice.Message
		if message.Content != "" {
			itemID := fmt.Sprintf("item_%s", uuid.New().String()[:29])
			if err := writeEvent("response.output_item.added", map[string]interface{}{
				"type": "response.output_item.added", "response_id": responseID, "output_index": outputIndex,
				"item": map[string]interface{}{
					"id": itemID, "type": "message", "role": "assistant",
					"status": "in_progress", "content": []interface{}{},
				},
			}); err != nil {
				return err
			}
			if err := writeEvent("response.content_part.added", map[string]interface{}{
				"type": "response.content_part.added", "response_id": responseID, "item_id": itemID,
				"output_index": outputIndex, "content_index": 0,
				"part": map[string]interface{}{"type": "output_text", "text": "", "annotations": []interface{}{}},
			}); err != nil {
				return err
			}
			if err := writeEvent("response.output_text.delta", map[string]interface{}{
				"type": "response.output_text.delta", "response_id": responseID, "item_id": itemID,
				"output_index": outputIndex, "content_index": 0, "delta": message.Content,
			}); err != nil {
				return err
			}
			if err := writeEvent("response.output_text.done", map[string]interface{}{
				"type": "response.output_text.done", "response_id": responseID, "item_id": itemID,
				"output_index": outputIndex, "content_index": 0, "text": message.Content,
			}); err != nil {
				return err
			}

			part := map[string]interface{}{"type": "output_text", "text": message.Content, "annotations": []interface{}{}}
			if err := writeEvent("response.content_part.done", map[string]interface{}{
				"type": "response.content_part.done", "response_id": responseID, "item_id": itemID,
				"output_index": outputIndex, "content_index": 0, "part": part,
			}); err != nil {
				return err
			}
			completedItem := map[string]interface{}{
				"id": itemID, "type": "message", "role": "assistant",
				"status": "completed", "content": []interface{}{part},
			}
			if err := writeEvent("response.output_item.done", map[string]interface{}{
				"type": "response.output_item.done", "response_id": responseID,
				"output_index": outputIndex, "item": completedItem,
			}); err != nil {
				return err
			}
			completedOutput = append(completedOutput, completedItem)
			outputIndex++
		}

		for _, toolCall := range message.ToolCalls {
			callID := toolCall.ID
			if callID == "" {
				callID = fmt.Sprintf("call_%s", uuid.New().String()[:24])
			}
			itemID := fmt.Sprintf("fc_%s", uuid.New().String()[:30])
			if err := writeEvent("response.output_item.added", map[string]interface{}{
				"type": "response.output_item.added", "response_id": responseID, "output_index": outputIndex,
				"item": map[string]interface{}{
					"id": itemID, "type": "function_call", "status": "in_progress",
					"call_id": callID, "name": toolCall.Function.Name, "arguments": "",
				},
			}); err != nil {
				return err
			}
			if toolCall.Function.Arguments != "" {
				if err := writeEvent("response.function_call_arguments.delta", map[string]interface{}{
					"type": "response.function_call_arguments.delta", "response_id": responseID, "item_id": itemID,
					"output_index": outputIndex, "delta": toolCall.Function.Arguments,
				}); err != nil {
					return err
				}
			}
			if err := writeEvent("response.function_call_arguments.done", map[string]interface{}{
				"type": "response.function_call_arguments.done", "response_id": responseID, "item_id": itemID,
				"output_index": outputIndex, "call_id": callID, "name": toolCall.Function.Name,
				"arguments": toolCall.Function.Arguments,
			}); err != nil {
				return err
			}
			completedItem := map[string]interface{}{
				"id": itemID, "type": "function_call", "status": "completed",
				"call_id": callID, "name": toolCall.Function.Name, "arguments": toolCall.Function.Arguments,
			}
			if err := writeEvent("response.output_item.done", map[string]interface{}{
				"type": "response.output_item.done", "response_id": responseID,
				"output_index": outputIndex, "item": completedItem,
			}); err != nil {
				return err
			}
			completedOutput = append(completedOutput, completedItem)
			outputIndex++
		}
	}

	completedResponse := map[string]interface{}{
		"id": responseID, "object": "response", "status": "completed",
		"created_at": createdAt, "model": model, "output": completedOutput,
	}
	if resp.Usage != nil {
		completedResponse["usage"] = chatUsageAsResponses(resp.Usage)
	}
	return writeEvent("response.completed", map[string]interface{}{
		"type": "response.completed", "response": completedResponse,
	})
}

func chatUsageAsResponses(usage *types.Usage) map[string]interface{} {
	result := map[string]interface{}{
		"input_tokens":  usage.PromptTokens,
		"output_tokens": usage.CompletionTokens,
		"total_tokens":  usage.TotalTokens,
	}
	if usage.PromptTokensDetails != nil {
		details := map[string]interface{}{
			"cached_tokens": usage.PromptTokensDetails.CachedTokens,
		}
		if usage.PromptTokensDetails.CacheWriteTokens != 0 {
			details["cache_write_tokens"] = usage.PromptTokensDetails.CacheWriteTokens
		}
		result["input_tokens_details"] = details
	}
	if usage.CompletionTokensDetails != nil {
		result["output_tokens_details"] = map[string]interface{}{
			"reasoning_tokens": usage.CompletionTokensDetails.ReasoningTokens,
		}
	}
	return result
}

// copyResponsesStreamAndExtractUsage forwards a Responses API SSE stream to the
// client while reading the usage object out of the terminal response.completed
// event. Before this existed the passthrough was a blind byte copy, so every
// Codex CLI request was logged with zero tokens.
//
// Scanning line-by-line rather than block-copying costs a little throughput; the
// alternative is having no usage data at all for the entire /v1/responses path.
func copyResponsesStreamAndExtractUsage(src io.Reader, dst io.Writer) (*types.ResponsesUsage, error) {
	flusher, canFlush := dst.(interface{ Flush() })
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var usage *types.ResponsesUsage

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "data: ") {
			var event map[string]interface{}
			if json.Unmarshal([]byte(line[6:]), &event) == nil {
				if t, _ := event["type"].(string); t == "response.completed" {
					if u := types.ParseResponsesUsage(event); u != nil {
						usage = u
					}
				}
			}
		}

		if _, err := io.WriteString(dst, line+"\n"); err != nil {
			return usage, err
		}
		if canFlush {
			flusher.Flush()
		}
	}

	return usage, scanner.Err()
}

// recordLog logs one /v1/responses request. usage may be nil (open failed, or
// the stream died before response.completed), in which case the token buckets
// stay at zero and reasoning at unknown.
func (h *ResponsesHandler) recordLog(c *gin.Context, model string, start time.Time, usage *types.TokenUsage, account string, failedOver []string, err error) {
	if h.statsDB == nil {
		return
	}
	entry := &stats.RequestLog{
		Time:            time.Now(),
		Model:           model,
		Backend:         h.servingBackend(c, model),
		LatencyMs:       time.Since(start).Milliseconds(),
		Stream:          true,
		Status:          http.StatusOK,
		APIKeyName:      apiKeyName(c),
		Account:         account,
		FailoverFrom:    strings.Join(mergeFailover(failedOver, c.Request.Context()), ","),
		ReasoningTokens: types.ReasoningUnknown,
	}
	if usage != nil {
		entry.SetUsage(*usage)
	}
	if err != nil {
		entry.Status = errStatus(err)
		entry.Error = err.Error()
	}
	if recordErr := h.statsDB.Record(entry); recordErr != nil {
		log.Printf("stats record error: %v", recordErr)
	}
}
