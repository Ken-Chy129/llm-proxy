package handler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/user/cli-proxy/internal/executor"
	"github.com/user/cli-proxy/internal/router"
	"github.com/user/cli-proxy/internal/stats"
)

type AnthropicHandler struct {
	router  *router.Router
	statsDB *stats.DB
}

func NewAnthropicHandler(r *router.Router, db *stats.DB) *AnthropicHandler {
	return &AnthropicHandler{router: r, statsDB: db}
}

func (h *AnthropicHandler) Messages(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		anthropicError(c, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}

	var meta struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		anthropicError(c, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	meta.Model = strings.TrimSpace(meta.Model)
	if meta.Model == "" {
		anthropicError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	exec, err := h.router.Resolve(meta.Model)
	if err != nil {
		anthropicError(c, http.StatusNotFound, "not_found_error", err.Error())
		return
	}

	ae, ok := exec.(executor.AnthropicExecutor)
	if !ok {
		anthropicError(c, http.StatusBadRequest, "invalid_request_error",
			"model "+meta.Model+" does not support Anthropic Messages API")
		return
	}

	start := time.Now()

	if meta.Stream {
		h.handleAnthropicStream(c, ae, meta.Model, body, start)
	} else {
		h.handleAnthropicRaw(c, ae, meta.Model, body, start)
	}
}

func (h *AnthropicHandler) handleAnthropicStream(c *gin.Context, ae executor.AnthropicExecutor, model string, body []byte, start time.Time) {
	stream, statusCode, err := ae.OpenAnthropicStream(c.Request.Context(), body, c.Request.Header)
	if err != nil {
		log.Printf("anthropic stream open error: %v", err)
		h.recordAnthropicLog(model, start, true, nil, err)
		anthropicError(c, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer stream.Close()

	if statusCode != http.StatusOK {
		errBody, _ := io.ReadAll(stream)
		h.recordAnthropicLog(model, start, true, nil, fmt.Errorf("upstream error %d", statusCode))
		c.Data(statusCode, "application/json", errBody)
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Writer.Flush()

	usage, copyErr := copyStreamAndExtractUsage(stream, c.Writer)
	h.recordAnthropicLog(model, start, true, usage, copyErr)
	if copyErr != nil {
		log.Printf("anthropic stream copy error: %v", copyErr)
	}
}

func (h *AnthropicHandler) handleAnthropicRaw(c *gin.Context, ae executor.AnthropicExecutor, model string, body []byte, start time.Time) {
	respBody, statusCode, err := ae.ExecuteAnthropicRaw(c.Request.Context(), body, c.Request.Header)
	if err != nil {
		h.recordAnthropicLog(model, start, false, nil, err)
		log.Printf("anthropic error: %v", err)
		anthropicError(c, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	usage := extractUsageFromResponse(respBody)
	h.recordAnthropicLog(model, start, false, usage, nil)
	c.Data(statusCode, "application/json", respBody)
}

func anthropicError(c *gin.Context, status int, errType, message string) {
	c.JSON(status, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func extractUsageFromResponse(body []byte) *anthropicUsage {
	var resp struct {
		Usage *anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Usage == nil {
		return nil
	}
	return resp.Usage
}

// copyStreamAndExtractUsage forwards an Anthropic SSE stream to the client
// while extracting token usage from message_start and message_delta events.
func copyStreamAndExtractUsage(src io.Reader, dst io.Writer) (*anthropicUsage, error) {
	flusher, canFlush := dst.(interface{ Flush() })
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var usage anthropicUsage

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "data: ") {
			data := line[6:]
			var evt struct {
				Type    string `json:"type"`
				Message *struct {
					Usage *anthropicUsage `json:"usage"`
				} `json:"message"`
				Usage *struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal([]byte(data), &evt) == nil {
				switch evt.Type {
				case "message_start":
					if evt.Message != nil && evt.Message.Usage != nil {
						usage.InputTokens = evt.Message.Usage.InputTokens
					}
				case "message_delta":
					if evt.Usage != nil {
						usage.OutputTokens = evt.Usage.OutputTokens
					}
				}
			}
		}

		if _, err := io.WriteString(dst, line+"\n"); err != nil {
			return &usage, err
		}
		if canFlush {
			flusher.Flush()
		}
	}

	if err := scanner.Err(); err != nil {
		return &usage, err
	}
	return &usage, nil
}

func (h *AnthropicHandler) recordAnthropicLog(model string, start time.Time, stream bool, usage *anthropicUsage, err error) {
	if h.statsDB == nil {
		return
	}
	entry := &stats.RequestLog{
		Time:      time.Now(),
		Model:     model,
		Backend:   h.router.BackendName(model),
		LatencyMs: time.Since(start).Milliseconds(),
		Stream:    stream,
		Status:    http.StatusOK,
	}
	if usage != nil {
		entry.PromptTokens = usage.InputTokens
		entry.CompletionTokens = usage.OutputTokens
	}
	if err != nil {
		entry.Status = http.StatusInternalServerError
		entry.Error = err.Error()
	}
	if recordErr := h.statsDB.Record(entry); recordErr != nil {
		log.Printf("stats record error: %v", recordErr)
	}
}
