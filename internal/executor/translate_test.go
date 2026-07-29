package executor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

// decodeBlocks unmarshals translated message content into typed blocks.
func decodeBlocks(t *testing.T, raw json.RawMessage) []types.AnthropicContentBlock {
	t.Helper()
	var blocks []types.AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("unmarshal blocks from %s: %v", raw, err)
	}
	return blocks
}

func TestToAnthropicRequestConvertsBase64ImageURL(t *testing.T) {
	req := &types.ChatCompletionRequest{
		Model: "claude-opus-5",
		Messages: []types.ChatMessage{{
			Role: "user",
			Content: json.RawMessage(`[
				{"type":"text","text":"what is this"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD","detail":"high"}}
			]`),
		}},
	}

	ar := ToAnthropicRequest(req, req.Model)
	if len(ar.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(ar.Messages))
	}

	blocks := decodeBlocks(t, ar.Messages[0].Content)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d: %s", len(blocks), ar.Messages[0].Content)
	}
	if blocks[0].Type != "text" || blocks[0].Text != "what is this" {
		t.Errorf("block 0 = %+v, want text block", blocks[0])
	}
	if blocks[1].Type != "image" {
		t.Fatalf("block 1 type = %q, want %q", blocks[1].Type, "image")
	}
	src := blocks[1].Source
	if src == nil {
		t.Fatal("block 1 source is nil")
	}
	if src.Type != "base64" || src.MediaType != "image/png" || src.Data != "QUJD" {
		t.Errorf("source = %+v, want base64/image/png/QUJD", *src)
	}
	if src.URL != "" {
		t.Errorf("source.URL = %q, want empty for base64 source", src.URL)
	}

	// The serialized payload must not leak the OpenAI tag that upstream rejects.
	if got := string(ar.Messages[0].Content); strings.Contains(got, `"image_url"`) {
		t.Errorf("translated content still contains image_url tag: %s", got)
	}
}

func TestToAnthropicRequestConvertsRemoteImageURL(t *testing.T) {
	req := &types.ChatCompletionRequest{
		Model: "claude-opus-5",
		Messages: []types.ChatMessage{{
			Role:    "user",
			Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":"https://example.com/a.jpg"}}]`),
		}},
	}

	blocks := decodeBlocks(t, ToAnthropicRequest(req, req.Model).Messages[0].Content)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Type != "image" {
		t.Fatalf("type = %q, want image", blocks[0].Type)
	}
	src := blocks[0].Source
	if src == nil || src.Type != "url" || src.URL != "https://example.com/a.jpg" {
		t.Errorf("source = %+v, want url source", src)
	}
	if src.Data != "" || src.MediaType != "" {
		t.Errorf("source = %+v, want no base64 fields for url source", *src)
	}
}

func TestToAnthropicRequestPassesThroughStringContent(t *testing.T) {
	req := &types.ChatCompletionRequest{
		Model: "claude-opus-5",
		Messages: []types.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"hello"`)},
			{Role: "assistant", Content: json.RawMessage(`"hi there"`)},
		},
	}

	ar := ToAnthropicRequest(req, req.Model)
	if len(ar.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(ar.Messages))
	}
	if got := string(ar.Messages[0].Content); got != `"hello"` {
		t.Errorf("user content = %s, want \"hello\"", got)
	}
	if got := string(ar.Messages[1].Content); got != `"hi there"` {
		t.Errorf("assistant content = %s, want \"hi there\"", got)
	}
}

func TestToAnthropicRequestTextOnlyArrayStaysTextBlocks(t *testing.T) {
	req := &types.ChatCompletionRequest{
		Model: "claude-opus-5",
		Messages: []types.ChatMessage{{
			Role:    "user",
			Content: json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`),
		}},
	}

	blocks := decodeBlocks(t, ToAnthropicRequest(req, req.Model).Messages[0].Content)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	for i, want := range []string{"a", "b"} {
		if blocks[i].Type != "text" || blocks[i].Text != want {
			t.Errorf("block %d = %+v, want text %q", i, blocks[i], want)
		}
	}
}

func TestToAnthropicRequestAssistantMultimodalContent(t *testing.T) {
	req := &types.ChatCompletionRequest{
		Model: "claude-opus-5",
		Messages: []types.ChatMessage{{
			Role:    "assistant",
			Content: json.RawMessage(`[{"type":"text","text":"see"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,WFla"}}]`),
		}},
	}

	blocks := decodeBlocks(t, ToAnthropicRequest(req, req.Model).Messages[0].Content)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[1].Type != "image" || blocks[1].Source == nil || blocks[1].Source.MediaType != "image/jpeg" {
		t.Errorf("block 1 = %+v, want jpeg image block", blocks[1])
	}
}

func TestImageSourceFromURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want *types.AnthropicMediaSource
	}{
		{
			name: "base64 png",
			url:  "data:image/png;base64,QUJD",
			want: &types.AnthropicMediaSource{Type: "base64", MediaType: "image/png", Data: "QUJD"},
		},
		{
			name: "base64 without media type defaults to png",
			url:  "data:;base64,QUJD",
			want: &types.AnthropicMediaSource{Type: "base64", MediaType: "image/png", Data: "QUJD"},
		},
		{
			name: "https url",
			url:  "https://example.com/x.png",
			want: &types.AnthropicMediaSource{Type: "url", URL: "https://example.com/x.png"},
		},
		{
			name: "http url",
			url:  "http://example.com/x.png",
			want: &types.AnthropicMediaSource{Type: "url", URL: "http://example.com/x.png"},
		},
		{name: "non base64 data uri dropped", url: "data:text/plain,hello", want: nil},
		{name: "empty base64 payload dropped", url: "data:image/png;base64,", want: nil},
		{name: "malformed data uri dropped", url: "data:image/png;base64", want: nil},
		{name: "unsupported scheme dropped", url: "file:///etc/passwd", want: nil},
		{name: "empty dropped", url: "", want: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := imageSourceFromURL(tc.url)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %+v, want nil", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("got nil, want %+v", *tc.want)
			}
			if *got != *tc.want {
				t.Errorf("got %+v, want %+v", *got, *tc.want)
			}
		})
	}
}

func TestContentPartImageURLAcceptsBareString(t *testing.T) {
	var parts []openAIContentPart
	raw := `[{"type":"input_image","image_url":"https://example.com/b.png"}]`
	if err := json.Unmarshal([]byte(raw), &parts); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := contentPartImageURL(parts[0]); got != "https://example.com/b.png" {
		t.Errorf("got %q, want the bare string url", got)
	}
}

func TestToAnthropicContentLeavesUnknownShapesAlone(t *testing.T) {
	// An array of unknown block types yields no convertible blocks, so the
	// original payload is returned rather than an empty array.
	raw := json.RawMessage(`[{"type":"video","foo":1}]`)
	if got := string(toAnthropicContent(raw)); got != string(raw) {
		t.Errorf("got %s, want passthrough %s", got, raw)
	}
	if got := string(toAnthropicContent(nil)); got != "" {
		t.Errorf("nil content = %q, want empty", got)
	}
}
