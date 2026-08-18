package handler

import (
	"bufio"
	"context"
	"encoding/json"
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

// anthropicErrStatus resolves a connection/setup error to the upstream status
// when known, else 502 (what the client receives for these failures).
func anthropicErrStatus(err error) int {
	if st := executor.StatusFromError(err); st != 0 {
		return st
	}
	return http.StatusBadGateway
}

func (h *AnthropicHandler) handleAnthropicStream(c *gin.Context, ae executor.AnthropicExecutor, model string, body []byte, start time.Time) {
	ctx, getAccount := executor.WithAccountRecorder(c.Request.Context())
	ctx, getBackend := executor.WithBackendRecorder(ctx)
	stream, statusCode, err := ae.OpenAnthropicStream(ctx, body, c.Request.Header)
	account, failedOver := getAccount()
	served := getBackend()
	if err != nil {
		log.Printf("anthropic stream open error: %v", err)
		h.recordAnthropicLog(c, model, start, true, nil, err, anthropicErrStatus(err), account, failedOver, served, ctx)
		anthropicError(c, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer stream.Close()

	if statusCode != http.StatusOK {
		errBody, _ := io.ReadAll(stream)
		h.recordAnthropicLog(c, model, start, true, nil, fmt.Errorf("upstream error %d", statusCode), statusCode, account, failedOver, served, ctx)
		c.Data(statusCode, "application/json", errBody)
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Writer.Flush()

	usage, copyErr := copyStreamAndExtractUsage(stream, c.Writer)
	h.recordAnthropicLog(c, model, start, true, usage, copyErr, http.StatusOK, account, failedOver, served, ctx)
	if copyErr != nil {
		log.Printf("anthropic stream copy error: %v", copyErr)
	}
}

func (h *AnthropicHandler) handleAnthropicRaw(c *gin.Context, ae executor.AnthropicExecutor, model string, body []byte, start time.Time) {
	ctx, getAccount := executor.WithAccountRecorder(c.Request.Context())
	ctx, getBackend := executor.WithBackendRecorder(ctx)
	respBody, statusCode, err := ae.ExecuteAnthropicRaw(ctx, body, c.Request.Header)
	account, failedOver := getAccount()
	served := getBackend()
	if err != nil {
		h.recordAnthropicLog(c, model, start, false, nil, err, anthropicErrStatus(err), account, failedOver, served, ctx)
		log.Printf("anthropic error: %v", err)
		anthropicError(c, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	if statusCode >= 400 {
		h.recordAnthropicLog(c, model, start, false, nil, fmt.Errorf("upstream error %d", statusCode), statusCode, account, failedOver, served, ctx)
		c.Data(statusCode, "application/json", respBody)
		return
	}
	usage := extractUsageFromResponse(respBody)
	h.recordAnthropicLog(c, model, start, false, usage, nil, statusCode, account, failedOver, served, ctx)
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

func extractUsageFromResponse(body []byte) *types.AnthropicUsage {
	var resp struct {
		Usage *types.AnthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Usage == nil {
		return nil
	}
	return resp.Usage
}

// copyStreamAndExtractUsage forwards an Anthropic SSE stream to the client
// while extracting token usage from message_start and message_delta events.
//
// message_start carries the input and cache counts, message_delta the output —
// and newer API versions repeat the whole object in the delta with the input
// fields zeroed. MergeNonZero is what stops that repeat from erasing the cache
// numbers, which is the difference between logging a 900K-token cached request
// as 900K and logging it as 12K.
func copyStreamAndExtractUsage(src io.Reader, dst io.Writer) (*types.AnthropicUsage, error) {
	flusher, canFlush := dst.(interface{ Flush() })
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var usage types.AnthropicUsage

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "data: ") {
			data := line[6:]
			var evt struct {
				Type    string `json:"type"`
				Message *struct {
					Usage *types.AnthropicUsage `json:"usage"`
				} `json:"message"`
				Usage *types.AnthropicUsage `json:"usage"`
			}
			if json.Unmarshal([]byte(data), &evt) == nil {
				switch evt.Type {
				case "message_start":
					if evt.Message != nil && evt.Message.Usage != nil {
						usage.MergeNonZero(*evt.Message.Usage)
					}
				case "message_delta":
					if evt.Usage != nil {
						usage.MergeNonZero(*evt.Usage)
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

func (h *AnthropicHandler) recordAnthropicLog(c *gin.Context, model string, start time.Time, stream bool, usage *types.AnthropicUsage, err error, status int, account string, failedOver []string, servedBackend string, ctx context.Context) {
	if h.statsDB == nil {
		return
	}
	// A fallback chain reports which backend actually served, which differs from
	// the routing table when the primary was exhausted. Without this, relay
	// overflow traffic would be logged as if the subscription had served it.
	backend := h.router.BackendName(model)
	if servedBackend != "" {
		backend = servedBackend
	}
	if from := executor.BackendFallbackFrom(ctx); len(from) > 0 {
		failedOver = append(failedOver, from...)
	}
	entry := &stats.RequestLog{
		Time:            time.Now(),
		Model:           model,
		Backend:         backend,
		LatencyMs:       time.Since(start).Milliseconds(),
		Stream:          stream,
		Status:          status,
		APIKeyName:      apiKeyName(c),
		Account:         account,
		FailoverFrom:    strings.Join(failedOver, ","),
		ReasoningTokens: types.ReasoningUnknown,
	}
	if usage != nil {
		entry.SetUsage(usage.Breakdown())
	}
	if err != nil {
		entry.Error = err.Error()
	}
	if recordErr := h.statsDB.Record(entry); recordErr != nil {
		log.Printf("stats record error: %v", recordErr)
	}
}
