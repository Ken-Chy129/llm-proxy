package handler

import (
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

type ChatHandler struct {
	router  *router.Router
	statsDB *stats.DB
	// catalog reports what a provider says it can serve, and is what bounds the
	// dashboard's probe path to real upstream models. Optional: nil disables
	// probing entirely rather than making it unbounded.
	catalog func(provider string) []string
}

func NewChatHandler(r *router.Router, db *stats.DB) *ChatHandler {
	return &ChatHandler{router: r, statsDB: db}
}

// SetCatalogSource wires in the provider catalog lookup, enabling the
// dashboard-only probe path. It is injected rather than taken in the
// constructor because the catalog lives on the admin handler, which is built
// after this one and owns the executors it reads.
func (h *ChatHandler) SetCatalogSource(fn func(provider string) []string) {
	h.catalog = fn
}

// dashboardSession reports whether the caller is an authenticated dashboard
// session as opposed to a managed /v1 API key. Only the flag set by the session
// branch of APIKeyAuth counts — a key could be *named* "dashboard".
func dashboardSession(c *gin.Context) bool {
	v, ok := c.Get("dashboard_session")
	if !ok {
		return false
	}
	flag, _ := v.(bool)
	return flag
}

// servingBackend reports which provider actually handled the request. It is the
// head of the model's chain unless the request failed over, in which case the
// recorder holds the provider that ended up serving — logging the head would
// credit a subscription for traffic a paid relay absorbed.
func (h *ChatHandler) servingBackend(model, recorded string) string {
	if recorded != "" {
		return recorded
	}
	return h.router.BackendName(model)
}

// mergeFailover combines per-account failover (recorded by the executor) with
// per-provider failover (recorded by the chain), which are two different reasons
// a request moved and are both worth seeing on the log line.
func mergeFailover(accounts []string, ctx context.Context) []string {
	if from := executor.BackendFallbackFrom(ctx); len(from) > 0 {
		return append(accounts, from...)
	}
	return accounts
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
	// An unpublished model named with an explicit provider is the dashboard's
	// reachability probe: it answers "would this work if I published it?", which
	// is the question that decides whether to publish. Restricted to a logged-in
	// session and to models the provider itself lists, so it widens what an
	// operator can try without widening what a managed key can reach.
	if err != nil && h.catalog != nil && dashboardSession(c) {
		if probeExec, probeModel, probeErr := h.router.ResolveProbe(req.Model, h.catalog); probeErr == nil {
			exec, err, req.Model = probeExec, nil, probeModel
		}
	}
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{"message": err.Error(), "type": "invalid_request_error", "code": "model_not_found"},
		})
		return
	}
	// "model@provider" is consumed entirely by routing. Past this point the
	// request carries the published name, because that is what an executor maps
	// onto its upstream id and what a log line and a price lookup key off —
	// forwarding the suffix upstream is a 400 from the provider.
	req.Model, _ = router.SplitModelProvider(req.Model)

	start := time.Now()

	if req.Stream {
		if support, ok := exec.(executor.StreamingSupport); ok && !support.SupportsStreaming() {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": gin.H{"message": "the selected backend supports non-streaming Chat Completions only", "type": "invalid_request_error"},
			})
			return
		}
		h.handleStream(c, exec, &req, start)
		return
	}

	ctx, getAccount := executor.WithAccountRecorder(c.Request.Context())
	ctx, getBackend := executor.WithBackendRecorder(ctx)
	resp, err := exec.Execute(ctx, &req)
	latency := time.Since(start)
	account, failedOver := getAccount()

	logEntry := &stats.RequestLog{
		Time:         time.Now(),
		Model:        req.Model,
		Backend:      h.servingBackend(req.Model, getBackend()),
		Stream:       false,
		APIKeyName:   apiKeyName(c),
		Account:      account,
		FailoverFrom: strings.Join(mergeFailover(failedOver, ctx), ","),
		// Until usage arrives, reasoning is unknown rather than zero — an errored
		// request has no answer, and 0 would claim the model thought nothing.
		ReasoningTokens: types.ReasoningUnknown,
	}

	if err != nil {
		log.Printf("execute error: %v", err)
		logEntry.Status = errStatus(err)
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
		logEntry.SetUsage(resp.Usage.Breakdown())
	}
	h.recordLog(logEntry)

	c.JSON(http.StatusOK, resp)
}

func (h *ChatHandler) handleStream(c *gin.Context, exec interface {
	ExecuteStream(ctx context.Context, req *types.ChatCompletionRequest, w io.Writer) (*types.Usage, error)
}, req *types.ChatCompletionRequest, start time.Time) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	c.Stream(func(w io.Writer) bool {
		ctx, getAccount := executor.WithAccountRecorder(c.Request.Context())
		ctx, getBackend := executor.WithBackendRecorder(ctx)
		usage, err := exec.ExecuteStream(ctx, req, w)
		latency := time.Since(start)
		account, failedOver := getAccount()

		logEntry := &stats.RequestLog{
			Time:            time.Now(),
			Model:           req.Model,
			Backend:         h.servingBackend(req.Model, getBackend()),
			LatencyMs:       latency.Milliseconds(),
			Stream:          true,
			Status:          http.StatusOK,
			APIKeyName:      apiKeyName(c),
			Account:         account,
			FailoverFrom:    strings.Join(mergeFailover(failedOver, ctx), ","),
			ReasoningTokens: types.ReasoningUnknown,
		}
		if usage != nil {
			logEntry.SetUsage(usage.Breakdown())
		}
		if err != nil {
			log.Printf("stream error: %v", err)
			logEntry.Status = errStatus(err)
			logEntry.Error = err.Error()
			errJSON, _ := json.Marshal(gin.H{"error": gin.H{"message": err.Error(), "type": "server_error"}})
			fmt.Fprintf(w, "data: %s\n\n", errJSON)
		}
		h.recordLog(logEntry)
		return false
	})
}

// errStatus maps an executor error to the upstream HTTP status when known,
// falling back to 500 for connection/internal failures.
func errStatus(err error) int {
	if s := executor.StatusFromError(err); s != 0 {
		return s
	}
	return http.StatusInternalServerError
}

func (h *ChatHandler) recordLog(entry *stats.RequestLog) {
	if h.statsDB != nil {
		if err := h.statsDB.Record(entry); err != nil {
			log.Printf("stats record error: %v", err)
		}
	}
}

// rewriteBodyModel replaces the "model" field of a raw JSON request body,
// which the passthrough endpoints (Responses, Anthropic Messages) need because
// they forward the caller's bytes rather than a re-marshalled struct: the
// "@provider" routing override lives in that field and must not travel
// upstream. Only the one key is touched, so every other field a provider cares
// about survives byte-for-byte.
func rewriteBodyModel(body []byte, model string) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse request body: %w", err)
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return nil, fmt.Errorf("encode model name: %w", err)
	}
	payload["model"] = encoded
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("rewrite model name: %w", err)
	}
	return rewritten, nil
}

func apiKeyName(c *gin.Context) string {
	if v, ok := c.Get("api_key_name"); ok {
		return v.(string)
	}
	return ""
}

// ListModels serves /v1/models. It lists only what can be served right now: a
// paused backend's models used to be advertised here and then rejected by
// Resolve, so clients built a model picker out of entries that always failed.
func (h *ChatHandler) ListModels(c *gin.Context) {
	models := h.router.UsableModels()
	data := make([]gin.H, len(models))
	for i, m := range models {
		data[i] = gin.H{
			"id":       m,
			"object":   "model",
			"owned_by": "llm-proxy",
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   data,
	})
}
