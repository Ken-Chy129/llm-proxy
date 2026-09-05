package auth

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// Verbatim shape of a /v1/models page, trimmed to the fields we read plus
// enough of the rest to prove the extra metadata is ignored rather than
// tripping the decoder.
const claudeModelsPayload = `{
  "data": [
    {"type":"model","id":"claude-fable-5-1","display_name":"Claude Fable 5.1","created_at":"2026-08-28T00:00:00Z","max_input_tokens":1000000,"max_tokens":128000,
     "capabilities":{"batch":{"supported":true},"thinking":{"supported":true,"types":{"adaptive":{"supported":true}}}}},
    {"type":"model","id":"claude-opus-5","display_name":"Claude Opus 5"},
    {"type":"model","id":"claude-haiku-4-5-20251001","display_name":"Claude Haiku 4.5"}
  ],
  "has_more": false,
  "first_id": "claude-fable-5-1"
}`

func TestParseClaudeModelsKeepsUpstreamOrderAndIgnoresMetadata(t *testing.T) {
	models, err := parseClaudeModels([]byte(claudeModelsPayload))
	if err != nil {
		t.Fatalf("parseClaudeModels() error: %v", err)
	}
	want := []string{"claude-fable-5-1", "claude-opus-5", "claude-haiku-4-5-20251001"}
	if !slices.Equal(models, want) {
		t.Fatalf("models = %v, want %v", models, want)
	}
}

// An empty page means the catalog is unusable; reporting it as success would
// silently blank the provider card.
func TestParseClaudeModelsRejectsAnEmptyCatalog(t *testing.T) {
	for name, body := range map[string]string{
		"no models": `{"data":[],"has_more":false}`,
		"blank ids": `{"data":[{"type":"model","id":"   "}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseClaudeModels([]byte(body)); err == nil {
				t.Error("an empty catalog was accepted")
			}
		})
	}
}

func TestParseClaudeModelsRejectsMalformedJSON(t *testing.T) {
	if _, err := parseClaudeModels([]byte(`{"data":`)); err == nil {
		t.Error("malformed JSON was accepted")
	}
}

// The headers are the fragile part of this call: /v1/models is gated on the
// Claude Code beta as well as the OAuth one, and sending only the OAuth beta
// returns a 401 that reads like an expired token.
func TestClaudeModelsRequestCarriesBothRequiredBetas(t *testing.T) {
	req, err := newClaudeModelsRequest(context.Background(), "sk-ant-oat01-test")
	if err != nil {
		t.Fatalf("newClaudeModelsRequest() error: %v", err)
	}

	betas := req.Header.Get("anthropic-beta")
	for _, want := range []string{claudeCodeBetaValue, claudeOAuthBetaValue} {
		if !strings.Contains(betas, want) {
			t.Errorf("anthropic-beta = %q, missing %q", betas, want)
		}
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-ant-oat01-test" {
		t.Errorf("Authorization = %q, want the OAuth token as a bearer", got)
	}
	if got := req.Header.Get("anthropic-version"); got == "" {
		t.Error("anthropic-version is required and was not set")
	}
	if got := req.Header.Get("x-app"); got != "cli" {
		t.Errorf("x-app = %q, want cli", got)
	}
	if !strings.HasPrefix(req.Header.Get("User-Agent"), "claude-cli/") {
		t.Errorf("User-Agent = %q, want the Claude Code identity", req.Header.Get("User-Agent"))
	}
	if req.URL.Query().Get("limit") == "" {
		t.Error("request does not ask for a full page, so a short default could truncate the catalog")
	}
}
