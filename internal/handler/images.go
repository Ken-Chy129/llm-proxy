package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

const imageGenTimeout = 5 * time.Minute

type ImagesHandler struct {
	router  *router.Router
	statsDB *stats.DB
}

func NewImagesHandler(r *router.Router, db *stats.DB) *ImagesHandler {
	return &ImagesHandler{router: r, statsDB: db}
}

type imageGenRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	Quality        string `json:"quality,omitempty"`
	Background     string `json:"background,omitempty"`
	OutputFormat   string `json:"output_format,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
}

func (h *ImagesHandler) ImagesGenerations(c *gin.Context) {
	var req imageGenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "invalid request: " + err.Error(), "type": "invalid_request_error"},
		})
		return
	}

	if req.Model == "" {
		req.Model = "gpt-image-2"
	}

	exec, err := h.findCodexExecutor()
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{"message": err.Error(), "type": "invalid_request_error"},
		})
		return
	}

	promptSnippet := req.Prompt
	if len(promptSnippet) > 100 {
		promptSnippet = promptSnippet[:100] + "..."
	}
	log.Printf("image generation: model=%s size=%s quality=%s prompt=%q", req.Model, req.Size, req.Quality, promptSnippet)

	start := time.Now()

	ctx, cancel := context.WithTimeout(c.Request.Context(), imageGenTimeout)
	defer cancel()

	codexReq := buildCodexImageRequest(&req)
	body, _ := json.Marshal(codexReq)

	var buf bytes.Buffer
	if err := exec.ExecuteRawStream(ctx, body, &buf); err != nil {
		latency := time.Since(start)
		if errors.Is(err, context.DeadlineExceeded) {
			log.Printf("image generation timeout after %s: model=%s prompt=%q", latency.Round(time.Second), req.Model, promptSnippet)
			h.recordLog(req.Model, start, fmt.Errorf("timeout after %s", latency.Round(time.Second)))
		} else {
			log.Printf("image generation error after %s: %v", latency.Round(time.Second), err)
			h.recordLog(req.Model, start, err)
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"message": err.Error(), "type": "server_error"},
		})
		return
	}

	resp, err := extractImageResponse(&buf, req.ResponseFormat)
	if err != nil {
		log.Printf("image extraction error: %v", err)
		h.recordLog(req.Model, start, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"message": err.Error(), "type": "server_error"},
		})
		return
	}

	latency := time.Since(start)
	log.Printf("image generation success: model=%s images=%d latency=%s", req.Model, len(resp.Data), latency.Round(time.Millisecond))
	h.recordLog(req.Model, start, nil)
	c.JSON(http.StatusOK, resp)
}

func (h *ImagesHandler) ImagesEdits(c *gin.Context) {
	prompt := c.PostForm("prompt")
	if prompt == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "prompt is required", "type": "invalid_request_error"},
		})
		return
	}

	file, header, err := c.Request.FormFile("image")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "image file is required: " + err.Error(), "type": "invalid_request_error"},
		})
		return
	}
	defer file.Close()

	imageBytes, err := io.ReadAll(file)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "failed to read image: " + err.Error(), "type": "invalid_request_error"},
		})
		return
	}

	mimeType := "image/png"
	if ct := header.Header.Get("Content-Type"); ct != "" {
		mimeType = ct
	} else {
		name := strings.ToLower(header.Filename)
		switch {
		case strings.HasSuffix(name, ".jpg"), strings.HasSuffix(name, ".jpeg"):
			mimeType = "image/jpeg"
		case strings.HasSuffix(name, ".webp"):
			mimeType = "image/webp"
		case strings.HasSuffix(name, ".gif"):
			mimeType = "image/gif"
		}
	}

	imageB64 := base64.StdEncoding.EncodeToString(imageBytes)
	imageDataURL := "data:" + mimeType + ";base64," + imageB64

	model := c.DefaultPostForm("model", "gpt-image-2")
	size := c.PostForm("size")
	quality := c.PostForm("quality")

	exec, err := h.findCodexExecutor()
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{"message": err.Error(), "type": "invalid_request_error"},
		})
		return
	}

	promptSnippet := prompt
	if len(promptSnippet) > 100 {
		promptSnippet = promptSnippet[:100] + "..."
	}
	log.Printf("image edit: model=%s size=%s quality=%s prompt=%q", model, size, quality, promptSnippet)

	start := time.Now()
	ctx, cancel := context.WithTimeout(c.Request.Context(), imageGenTimeout)
	defer cancel()

	codexReq := buildCodexImageEditRequest(model, prompt, imageDataURL, size, quality)
	body, _ := json.Marshal(codexReq)

	var buf bytes.Buffer
	if err := exec.ExecuteRawStream(ctx, body, &buf); err != nil {
		latency := time.Since(start)
		if errors.Is(err, context.DeadlineExceeded) {
			log.Printf("image edit timeout after %s: model=%s prompt=%q", latency.Round(time.Second), model, promptSnippet)
			h.recordLog(model, start, fmt.Errorf("timeout after %s", latency.Round(time.Second)))
		} else {
			log.Printf("image edit error after %s: %v", latency.Round(time.Second), err)
			h.recordLog(model, start, err)
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"message": err.Error(), "type": "server_error"},
		})
		return
	}

	resp, err := extractImageResponse(&buf, "b64_json")
	if err != nil {
		log.Printf("image edit extraction error: %v", err)
		h.recordLog(model, start, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"message": err.Error(), "type": "server_error"},
		})
		return
	}

	latency := time.Since(start)
	log.Printf("image edit success: model=%s images=%d latency=%s", model, len(resp.Data), latency.Round(time.Millisecond))
	h.recordLog(model, start, nil)
	c.JSON(http.StatusOK, resp)
}

func buildCodexImageEditRequest(model, prompt, imageDataURL, size, quality string) *codexImageReq {
	tool := map[string]interface{}{
		"type":  "image_generation",
		"model": model,
	}
	if size != "" {
		tool["size"] = size
	}
	if quality != "" {
		tool["quality"] = quality
	}

	content := []interface{}{
		map[string]interface{}{
			"type":      "input_image",
			"image_url": imageDataURL,
		},
		map[string]interface{}{
			"type": "input_text",
			"text": prompt,
		},
	}

	return &codexImageReq{
		Model:        "gpt-5.4-mini",
		Instructions: "",
		Input: []interface{}{
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": content,
			},
		},
		Tools:             []interface{}{tool},
		ToolChoice:        map[string]string{"type": "image_generation"},
		Stream:            true,
		Store:             false,
		ParallelToolCalls: true,
	}
}

func (h *ImagesHandler) findCodexExecutor() (*executor.CodexExecutor, error) {
	// Try known codex models
	for _, model := range []string{"gpt-5.4-mini", "gpt-5.4", "gpt-5.5"} {
		exec, err := h.router.Resolve(model)
		if err == nil {
			if ce, ok := exec.(*executor.CodexExecutor); ok {
				return ce, nil
			}
		}
	}
	return nil, fmt.Errorf("no codex executor available for image generation")
}

type codexImageReq struct {
	Model             string        `json:"model"`
	Instructions      string        `json:"instructions"`
	Input             []interface{} `json:"input"`
	Tools             []interface{} `json:"tools"`
	ToolChoice        interface{}   `json:"tool_choice"`
	Stream            bool          `json:"stream"`
	Store             bool          `json:"store"`
	ParallelToolCalls bool          `json:"parallel_tool_calls"`
}

func buildCodexImageRequest(req *imageGenRequest) *codexImageReq {
	// Build image generation tool
	tool := map[string]interface{}{
		"type":   "image_generation",
		"action": "generate",
		"model":  req.Model,
	}
	if req.Size != "" {
		tool["size"] = req.Size
	}
	if req.Quality != "" {
		tool["quality"] = req.Quality
	}
	if req.Background != "" {
		tool["background"] = req.Background
	}
	if req.OutputFormat != "" {
		tool["output_format"] = req.OutputFormat
	}

	return &codexImageReq{
		Model:        "gpt-5.4-mini",
		Instructions: "",
		Input: []interface{}{
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{
						"type": "input_text",
						"text": req.Prompt,
					},
				},
			},
		},
		Tools:             []interface{}{tool},
		ToolChoice:        map[string]string{"type": "image_generation"},
		Stream:            true,
		Store:             false,
		ParallelToolCalls: true,
	}
}

type imageAPIResponse struct {
	Created int64              `json:"created"`
	Data    []imageAPIDataItem `json:"data"`
}

type imageAPIDataItem struct {
	B64JSON       string `json:"b64_json,omitempty"`
	URL           string `json:"url,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

func extractImageResponse(buf *bytes.Buffer, responseFormat string) (*imageAPIResponse, error) {
	resp := &imageAPIResponse{
		Created: time.Now().Unix(),
		Data:    []imageAPIDataItem{},
	}

	// Parse SSE: Codex uses "event: type\ndata: json\n\n" format
	// The response.completed line can be very large (contains base64 image)
	raw := buf.Bytes()
	events := splitSSEEvents(raw)

	for _, eventData := range events {
		var event map[string]interface{}
		if err := json.Unmarshal(eventData, &event); err != nil {
			continue
		}

		eventType, _ := event["type"].(string)

		// Extract from response.output_item.done (preferred, appears before response.completed)
		if eventType == "response.output_item.done" {
			item, _ := event["item"].(map[string]interface{})
			if item != nil {
				extractImageItem(item, responseFormat, resp)
			}
			continue
		}

		// Fallback: extract from response.completed
		if eventType == "response.completed" {
			response, _ := event["response"].(map[string]interface{})
			if response == nil {
				continue
			}
			if createdAt, ok := response["created_at"].(float64); ok && createdAt > 0 {
				resp.Created = int64(createdAt)
			}
			output, _ := response["output"].([]interface{})
			for _, item := range output {
				if itemMap, ok := item.(map[string]interface{}); ok {
					extractImageItem(itemMap, responseFormat, resp)
				}
			}
		}
	}

	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("no image generated (parsed %d SSE events)", len(events))
	}
	return resp, nil
}

func extractImageItem(item map[string]interface{}, responseFormat string, resp *imageAPIResponse) {
	itemType, _ := item["type"].(string)
	if itemType != "image_generation_call" {
		return
	}
	result, _ := item["result"].(string)
	if result == "" {
		return
	}

	entry := imageAPIDataItem{}
	if responseFormat == "url" {
		outputFmt, _ := item["output_format"].(string)
		mimeType := "image/png"
		if outputFmt == "jpeg" {
			mimeType = "image/jpeg"
		} else if outputFmt == "webp" {
			mimeType = "image/webp"
		}
		entry.URL = "data:" + mimeType + ";base64," + result
	} else {
		entry.B64JSON = result
	}
	if rp, ok := item["revised_prompt"].(string); ok {
		entry.RevisedPrompt = rp
	}
	resp.Data = append(resp.Data, entry)
}

// splitSSEEvents splits raw SSE bytes into individual JSON data payloads.
// Handles both "data: json" and "event: type\ndata: json" formats.
func splitSSEEvents(raw []byte) [][]byte {
	var events [][]byte
	lines := bytes.Split(raw, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data: ")) {
			data := bytes.TrimPrefix(line, []byte("data: "))
			if len(data) > 0 && data[0] == '{' {
				events = append(events, data)
			}
		}
	}
	return events
}

func (h *ImagesHandler) recordLog(model string, start time.Time, err error) {
	if h.statsDB == nil {
		return
	}
	entry := &stats.RequestLog{
		Time:      time.Now(),
		Model:     model,
		Backend:   "codex",
		LatencyMs: time.Since(start).Milliseconds(),
		Stream:    false,
		Status:    http.StatusOK,
	}
	if err != nil {
		entry.Status = http.StatusInternalServerError
		entry.Error = err.Error()
	}
	h.statsDB.Record(entry)
}

