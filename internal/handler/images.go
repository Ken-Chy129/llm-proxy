package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/user/cli-proxy/internal/executor"
	"github.com/user/cli-proxy/internal/router"
	"github.com/user/cli-proxy/internal/stats"
)

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

	// Find a codex executor
	exec, err := h.findCodexExecutor()
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{"message": err.Error(), "type": "invalid_request_error"},
		})
		return
	}

	start := time.Now()

	codexReq := buildCodexImageRequest(&req)
	body, _ := json.Marshal(codexReq)

	var buf bytes.Buffer
	if err := exec.ExecuteRawStream(c.Request.Context(), body, &buf); err != nil {
		log.Printf("image generation error: %v", err)
		h.recordLog(req.Model, start, err)
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

	h.recordLog(req.Model, start, nil)
	c.JSON(http.StatusOK, resp)
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

