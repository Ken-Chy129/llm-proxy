package incident

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const requestBodyLimit = 32 << 20

type resultWriter struct {
	gin.ResponseWriter
	body      bytes.Buffer
	truncated bool
}

func (w *resultWriter) Write(p []byte) (int, error) {
	before := w.body.Len()
	if w.body.Len() < requestBodyLimit {
		remaining := requestBodyLimit - w.body.Len()
		if len(p) < remaining {
			remaining = len(p)
		}
		w.body.Write(p[:remaining])
	}
	if before+len(p) > requestBodyLimit {
		w.truncated = true
	}
	return w.ResponseWriter.Write(p)
}

// CaptureFailures returns middleware that writes one private reproduction
// bundle for every failed /v1 request except rate limits and client cancels.
func CaptureFailures(cfg config.FailureCaptureConfig) gin.HandlerFunc {
	if !cfg.Enabled {
		return func(c *gin.Context) { c.Next() }
	}
	dir := strings.TrimSpace(cfg.Dir)
	if dir == "" {
		dir = "failure-captures"
	}
	days := cfg.RetentionDays
	if days <= 0 {
		days = 7
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("[failure-capture] disabled: create %s: %v", dir, err)
		return func(c *gin.Context) { c.Next() }
	}
	_ = os.Chmod(dir, 0o700)
	cleanup(dir, time.Duration(days)*24*time.Hour)
	log.Printf("[failure-capture] enabled dir=%s retention=%dd (bundles contain complete prompts)", dir, days)

	return func(c *gin.Context) {
		if !strings.HasPrefix(c.Request.URL.Path, "/v1/") {
			c.Next()
			return
		}
		request, readErr := io.ReadAll(io.LimitReader(c.Request.Body, requestBodyLimit+1))
		if readErr != nil || len(request) > requestBodyLimit {
			c.Next()
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(request))
		writer := &resultWriter{ResponseWriter: c.Writer}
		c.Writer = writer
		c.Next()

		status := c.Writer.Status()
		captureErr, _ := c.Get("failure_capture_error")
		errorText, _ := captureErr.(string)
		if errorText == "" && status < 400 {
			return
		}
		// A 429 is not necessarily quota: providers also use it for transient
		// overloaded/capacity faults, which are exactly the incidents we need to
		// reproduce. Exclude only bodies that identify an actual usage/rate limit.
		if isRateLimit(errorText, writer.body.Bytes()) {
			return
		}
		if c.Request.Context().Err() != nil || strings.Contains(strings.ToLower(errorText), "context canceled") {
			return
		}
		if errorText == "" {
			errorText = responseError(writer.body.Bytes())
		}
		save(dir, c, request, writer.body.Bytes(), writer.truncated, status, errorText)
	}
}

// MarkFailure records a stream or internal failure that happened after HTTP
// headers were committed and therefore is invisible in ResponseWriter.Status.
func MarkFailure(c *gin.Context, err error) {
	if err != nil {
		c.Set("failure_capture_error", err.Error())
	}
}

type metadata struct {
	CapturedAt        string                 `json:"captured_at"`
	Method            string                 `json:"method"`
	Path              string                 `json:"path"`
	Protocol          string                 `json:"protocol"`
	Model             string                 `json:"model,omitempty"`
	Backend           string                 `json:"backend,omitempty"`
	FailoverFrom      []string               `json:"failover_from,omitempty"`
	Attempts          []types.FailureAttempt `json:"attempts,omitempty"`
	Status            int                    `json:"status"`
	Error             string                 `json:"error,omitempty"`
	RequestBytes      int                    `json:"request_bytes"`
	ResponseBytes     int                    `json:"captured_response_bytes"`
	ResponseTruncated bool                   `json:"response_truncated"`
	RequestHeaders    http.Header            `json:"request_headers"`
	ResponseHeaders   http.Header            `json:"response_headers"`
}

func save(dir string, c *gin.Context, request, response []byte, responseTruncated bool, status int, errorText string) {
	now := time.Now().UTC()
	name := now.Format("20060102T150405.000000000Z") + "-" + uuid.NewString()[:8]
	tmp, final := filepath.Join(dir, "."+name+".tmp"), filepath.Join(dir, name)
	if err := os.Mkdir(tmp, 0o700); err != nil {
		log.Printf("[failure-capture] create: %v", err)
		return
	}
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(request, &payload)
	backend, _ := c.Get("failure_capture_backend")
	failover, _ := c.Get("failure_capture_failover")
	m := metadata{
		CapturedAt: now.Format(time.RFC3339Nano), Method: c.Request.Method, Path: c.Request.URL.Path,
		Protocol: protocol(c.Request.URL.Path), Model: payload.Model, Backend: stringValue(backend),
		FailoverFrom: stringsValue(failover), Attempts: executor.FailureAttempts(c.Request.Context()), Status: status, Error: errorText,
		RequestBytes: len(request), ResponseBytes: len(response), ResponseTruncated: responseTruncated,
		RequestHeaders: safeHeaders(c.Request.Header), ResponseHeaders: safeHeaders(c.Writer.Header()),
	}
	meta, _ := json.MarshalIndent(m, "", "  ")
	files := []struct {
		name string
		data []byte
	}{{"metadata.json", append(meta, '\n')}, {"request.json", request}, {"response.body", response}}
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(tmp, file.name), file.data, 0o600); err != nil {
			os.RemoveAll(tmp)
			return
		}
	}
	replay := fmt.Sprintf("#!/bin/sh\nset -eu\n: \"${LLM_PROXY_API_KEY:?set LLM_PROXY_API_KEY}\"\ncurl -N --fail-with-body \"${LLM_PROXY_BASE_URL:?set LLM_PROXY_BASE_URL}%s\" \\\n+  -H \"Authorization: Bearer $LLM_PROXY_API_KEY\" \\\n+  -H \"Content-Type: application/json\" \\\n+  --data-binary @request.json\n", c.Request.URL.Path)
	replay = strings.ReplaceAll(replay, "\n+  ", "\n  ")
	if err := os.WriteFile(filepath.Join(tmp, "replay.sh"), []byte(replay), 0o700); err != nil {
		os.RemoveAll(tmp)
		return
	}
	if err := os.Rename(tmp, final); err != nil {
		os.RemoveAll(tmp)
		return
	}
	log.Printf("[failure-capture] saved protocol=%s model=%s backend=%s status=%d path=%s", m.Protocol, m.Model, m.Backend, status, final)
}

func cleanup(dir string, retention time.Duration) {
	entries, _ := os.ReadDir(dir)
	cutoff := time.Now().Add(-retention)
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && entry.IsDir() && info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(dir, entry.Name()))
		}
	}
}
func protocol(path string) string {
	if strings.HasSuffix(path, "/messages") {
		return "anthropic_messages"
	}
	if strings.HasSuffix(path, "/responses") {
		return "openai_responses"
	}
	if strings.Contains(path, "/images/") {
		return "openai_images"
	}
	return "openai_chat_completions"
}
func responseError(body []byte) string {
	var v map[string]interface{}
	if json.Unmarshal(body, &v) == nil {
		if e, ok := v["error"].(map[string]interface{}); ok {
			if m, ok := e["message"].(string); ok {
				return m
			}
		}
		if e, ok := v["error"].(string); ok {
			return e
		}
	}
	return strings.TrimSpace(string(body))
}
func isRateLimit(errText string, body []byte) bool {
	s := strings.ToLower(errText + " " + string(body))
	return strings.Contains(s, "usage_limit_reached") ||
		strings.Contains(s, "rate_limit_error") ||
		strings.Contains(s, "rate limit has been reached") ||
		strings.Contains(s, "rate limit exceeded") ||
		strings.Contains(s, "quota exceeded")
}
func safeHeaders(h http.Header) http.Header {
	out := make(http.Header)
	for _, name := range []string{"Accept", "Content-Type", "Anthropic-Version", "Anthropic-Beta", "User-Agent", "Request-Id", "X-Request-Id"} {
		if v := h.Values(name); len(v) > 0 {
			out[name] = append([]string(nil), v...)
		}
	}
	return out
}
func stringValue(v interface{}) string    { s, _ := v.(string); return s }
func stringsValue(v interface{}) []string { s, _ := v.([]string); return s }
