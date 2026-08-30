package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/Ken-Chy129/llm-proxy/internal/executor"
	"github.com/Ken-Chy129/llm-proxy/internal/router"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
	"github.com/gin-gonic/gin"
)

func TestEncodeDecodeCompactSummary(t *testing.T) {
	enc := encodeCompactSummary("  built cafe landing page  ")
	got, ok := decodeCompactSummary(enc)
	if !ok || got != "built cafe landing page" {
		t.Fatalf("roundtrip = %q ok=%v", got, ok)
	}
	if _, ok := decodeCompactSummary("not-base64"); ok {
		t.Fatal("garbage should not decode")
	}
}

func TestDecodeCompactSummaryRejectsForeignPrefix(t *testing.T) {
	enc := "Y2xpcHJveHktY2xhdWRlLWNvbXBhY3QtdjE6aGVsbG8=" // cliproxy-claude-compact-v1:hello
	if _, ok := decodeCompactSummary(enc); ok {
		t.Fatal("foreign prefix should not decode")
	}
}

func TestTrailingCompactionTrigger(t *testing.T) {
	if !trailingCompactionTrigger(json.RawMessage(`[{"role":"user","content":"hi"},{"type":"compaction_trigger"}]`)) {
		t.Fatal("expected trailing trigger")
	}
	if trailingCompactionTrigger(json.RawMessage(`[{"type":"compaction_trigger"},{"role":"user","content":"hi"}]`)) {
		t.Fatal("non-trailing trigger must not start compact v2")
	}
	if trailingCompactionTrigger(json.RawMessage(`[{"type":"compaction","encrypted_content":"x"}]`)) {
		t.Fatal("replayed compaction item is not a trigger")
	}
}

func TestResponsesRequestSkipsTriggerAndReplaysCompactionSummary(t *testing.T) {
	enc := encodeCompactSummary("prior work: auth login")
	input := json.RawMessage(`[
		{"role":"user","content":[{"type":"input_text","text":"continue"}]},
		{"type":"compaction","id":"cmp_1","encrypted_content":"` + enc + `"},
		{"type":"compaction_trigger"}
	]`)
	chatReq, err := (&ResponsesHandler{}).toChatCompletionRequest(&responsesRequest{
		Model: "gpt-5.6-sol",
		Input: input,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(chatReq.Messages) != 2 {
		t.Fatalf("messages = %+v", chatReq.Messages)
	}
	var first []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(chatReq.Messages[0].Content, &first); err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Text != "continue" {
		t.Fatalf("first message = %+v", first)
	}
	var summary string
	if err := json.Unmarshal(chatReq.Messages[1].Content, &summary); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "prior work: auth login") {
		t.Fatalf("replayed summary = %q", summary)
	}
}

func TestResponsesCompactionTriggerReturnsExactlyOneCompactionItem(t *testing.T) {
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")
	var upstream types.ChatCompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&upstream); err != nil {
			t.Fatalf("decode upstream: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-anygen","object":"chat.completion","created":1,"model":"gpt-5.6-sol","choices":[{"index":0,"message":{"role":"assistant","content":"User is building a cafe site; next add booking."},"finish_reason":"stop"}],"usage":{"prompt_tokens":80,"completion_tokens":12,"total_tokens":92}}`)
	}))
	defer server.Close()

	anygenExec := executor.NewAnyGenExecutor(config.AnyGenConfig{
		Enabled:   true,
		BaseURL:   server.URL + "/api/v1",
		APIKeyEnv: "TEST_ANYGEN_LLM_KEY",
	})
	anygenExec.SetServed([]string{"gpt-5.6-sol"})
	r := router.New()
	r.SetProvider("anygen", anygenExec)
	r.SetRoutes([]router.Route{{Model: "gpt-5.6-sol", Providers: []string{"anygen"}}})
	h := NewResponsesHandler(r, nil)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"gpt-5.6-sol",
		"stream":true,
		"input":[
			{"role":"user","content":[{"type":"input_text","text":"build a cafe site"}]},
			{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"},
			{"type":"compaction_trigger"}
		],
		"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]
	}`))
	h.HandleResponses(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if upstream.Stream {
		t.Fatal("compaction summarization must be non-streaming")
	}
	if len(upstream.Tools) != 0 {
		t.Fatalf("tools leaked into compaction request: %+v", upstream.Tools)
	}
	if len(upstream.Messages) == 0 {
		t.Fatal("compaction request has no messages")
	}
	var prompt string
	if err := json.Unmarshal(upstream.Messages[len(upstream.Messages)-1].Content, &prompt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Summarize the conversation") {
		t.Fatalf("missing summarize prompt: %q", prompt)
	}

	body := w.Body.String()
	if strings.Contains(body, `"type":"message"`) {
		t.Fatalf("compaction SSE leaked a message item:\n%s", body)
	}
	if !strings.Contains(body, `"type":"compaction"`) || !strings.Contains(body, `"encrypted_content"`) {
		t.Fatalf("missing compaction item:\n%s", body)
	}
	assertTextInOrder(t, body,
		"event: response.created",
		"event: response.output_item.done",
		"event: response.completed",
	)

	completed := lastCompletedOutput(t, body)
	if len(completed) != 1 {
		t.Fatalf("completed output len = %d, want 1: %+v", len(completed), completed)
	}
	if typ, _ := completed[0]["type"].(string); typ != "compaction" {
		t.Fatalf("output[0].type = %q, want compaction", typ)
	}
	enc, _ := completed[0]["encrypted_content"].(string)
	summary, ok := decodeCompactSummary(enc)
	if !ok || !strings.Contains(summary, "cafe site") {
		t.Fatalf("decoded summary = %q ok=%v", summary, ok)
	}
	assertConsistentResponseID(t, body)
}

func TestResponsesCompactionEmptySummaryFailsBeforeSSE(t *testing.T) {
	t.Setenv("TEST_ANYGEN_LLM_KEY", "sk-ag-test")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-anygen","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	anygenExec := executor.NewAnyGenExecutor(config.AnyGenConfig{
		BaseURL:   server.URL + "/api/v1",
		APIKeyEnv: "TEST_ANYGEN_LLM_KEY",
	})
	anygenExec.SetServed([]string{"gpt-5.6-sol"})
	r := router.New()
	r.SetProvider("anygen", anygenExec)
	r.SetRoutes([]router.Route{{Model: "gpt-5.6-sol", Providers: []string{"anygen"}}})
	h := NewResponsesHandler(r, nil)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"gpt-5.6-sol",
		"stream":true,
		"input":[{"role":"user","content":"hi"},{"type":"compaction_trigger"}]
	}`))
	h.HandleResponses(c)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, empty summary must fail before SSE", got)
	}
	if !strings.Contains(w.Body.String(), "no summary") {
		t.Fatalf("error body = %s", w.Body.String())
	}
}

func lastCompletedOutput(t *testing.T, stream string) []map[string]interface{} {
	t.Helper()
	var output []map[string]interface{}
	for _, block := range strings.Split(stream, "\n\n") {
		var dataLine string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				dataLine = strings.TrimPrefix(line, "data: ")
				break
			}
		}
		if dataLine == "" {
			continue
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(dataLine), &event); err != nil {
			continue
		}
		if typ, _ := event["type"].(string); typ != "response.completed" {
			continue
		}
		resp, _ := event["response"].(map[string]interface{})
		raw, _ := resp["output"].([]interface{})
		output = output[:0]
		for _, item := range raw {
			obj, _ := item.(map[string]interface{})
			output = append(output, obj)
		}
	}
	return output
}
