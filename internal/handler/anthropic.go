package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
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
		h.recordAnthropicLog(model, start, true, err)
		anthropicError(c, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer stream.Close()

	if statusCode != http.StatusOK {
		errBody, _ := io.ReadAll(stream)
		h.recordAnthropicLog(model, start, true, fmt.Errorf("upstream error %d", statusCode))
		c.Data(statusCode, "application/json", errBody)
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Writer.Flush()

	copyErr := copyWithFlush(stream, c.Writer)
	h.recordAnthropicLog(model, start, true, copyErr)
	if copyErr != nil {
		log.Printf("anthropic stream copy error: %v", copyErr)
	}
}

func (h *AnthropicHandler) handleAnthropicRaw(c *gin.Context, ae executor.AnthropicExecutor, model string, body []byte, start time.Time) {
	respBody, statusCode, err := ae.ExecuteAnthropicRaw(c.Request.Context(), body, c.Request.Header)
	h.recordAnthropicLog(model, start, false, err)
	if err != nil {
		log.Printf("anthropic error: %v", err)
		anthropicError(c, http.StatusBadGateway, "api_error", err.Error())
		return
	}
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

func (h *AnthropicHandler) recordAnthropicLog(model string, start time.Time, stream bool, err error) {
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
	if err != nil {
		entry.Status = http.StatusInternalServerError
		entry.Error = err.Error()
	}
	if recordErr := h.statsDB.Record(entry); recordErr != nil {
		log.Printf("stats record error: %v", recordErr)
	}
}
