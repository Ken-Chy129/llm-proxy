package handler

import (
	"encoding/json"
	"testing"
)

func TestResponsesRequestConvertsCodexContentAndToolsToChatCompletions(t *testing.T) {
	input := json.RawMessage(`[
		{"role":"user","content":[{"type":"input_text","text":"inspect this repository"}]},
		{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"pwd\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"/tmp/project"}
	]`)
	tool := json.RawMessage(`{"type":"function","name":"shell","description":"Run a shell command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}`)
	req := &responsesRequest{
		Model: "kimi-k3",
		Input: input,
		Tools: []json.RawMessage{tool},
	}

	chatReq := (&ResponsesHandler{}).toChatCompletionRequest(req)
	if len(chatReq.Messages) != 3 {
		t.Fatalf("messages = %+v", chatReq.Messages)
	}

	var content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(chatReq.Messages[0].Content, &content); err != nil {
		t.Fatalf("decode converted content: %v", err)
	}
	if len(content) != 1 || content[0].Type != "text" || content[0].Text != "inspect this repository" {
		t.Fatalf("converted content = %+v", content)
	}

	if len(chatReq.Tools) != 1 {
		t.Fatalf("tools = %+v", chatReq.Tools)
	}
	if got := chatReq.Tools[0].Function.Name; got != "shell" {
		t.Fatalf("tool name = %q", got)
	}
	if len(chatReq.Tools[0].Function.Parameters) == 0 {
		t.Fatal("tool parameters were dropped")
	}
}
