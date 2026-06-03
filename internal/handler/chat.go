package handler

import (
	"context"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/user/cli-proxy/internal/router"
	"github.com/user/cli-proxy/internal/stats"
	"github.com/user/cli-proxy/internal/types"
)

type ChatHandler struct {
	router  *router.Router
	statsDB *stats.DB
}

func NewChatHandler(r *router.Router, db *stats.DB) *ChatHandler {
	return &ChatHandler{router: r, statsDB: db}
}

func (h *ChatHandler) ChatCompletions(c *gin.Context) {
	var req types.ChatCompletionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
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

	if req.Stream {
		h.handleStream(c, exec, &req, start)
		return
	}

	resp, err := exec.Execute(c.Request.Context(), &req)
	latency := time.Since(start)

	logEntry := &stats.RequestLog{
		Time:    time.Now(),
		Model:   req.Model,
		Backend: h.router.BackendName(req.Model),
		Stream:  false,
	}

	if err != nil {
		log.Printf("execute error: %v", err)
		logEntry.Status = http.StatusInternalServerError
		logEntry.LatencyMs = latency.Milliseconds()
		logEntry.Error = err.Error()
		h.recordLog(logEntry)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"message": err.Error(), "type": "server_error"},
		})
		return
	}

	logEntry.Status = http.StatusOK
	logEntry.LatencyMs = latency.Milliseconds()
	if resp.Usage != nil {
		logEntry.PromptTokens = resp.Usage.PromptTokens
		logEntry.CompletionTokens = resp.Usage.CompletionTokens
	}
	h.recordLog(logEntry)

	c.JSON(http.StatusOK, resp)
}

func (h *ChatHandler) handleStream(c *gin.Context, exec interface {
	ExecuteStream(ctx context.Context, req *types.ChatCompletionRequest, w io.Writer) error
}, req *types.ChatCompletionRequest, start time.Time) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	c.Stream(func(w io.Writer) bool {
		err := exec.ExecuteStream(c.Request.Context(), req, w)
		latency := time.Since(start)

		logEntry := &stats.RequestLog{
			Time:      time.Now(),
			Model:     req.Model,
			Backend:   h.router.BackendName(req.Model),
			LatencyMs: latency.Milliseconds(),
			Stream:    true,
			Status:    http.StatusOK,
		}
		if err != nil {
			log.Printf("stream error: %v", err)
			logEntry.Status = http.StatusInternalServerError
			logEntry.Error = err.Error()
		}
		h.recordLog(logEntry)
		return false
	})
}

func (h *ChatHandler) recordLog(entry *stats.RequestLog) {
	if h.statsDB != nil {
		if err := h.statsDB.Record(entry); err != nil {
			log.Printf("stats record error: %v", err)
		}
	}
}

func (h *ChatHandler) ListModels(c *gin.Context) {
	models := h.router.AllModels()
	data := make([]gin.H, len(models))
	for i, m := range models {
		data[i] = gin.H{
			"id":       m,
			"object":   "model",
			"owned_by": "cli-proxy",
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   data,
	})
}
