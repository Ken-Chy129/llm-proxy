package auth

import (
	"strings"
	"testing"
)

// The catalog upstream returns is gated on the client_version we claim, so a
// stale constant silently pins the dashboard to an older generation of models.
func TestCodexClientVersionIsCurrent(t *testing.T) {
	if CodexClientVersion == "0.135.0" {
		t.Fatal("client version is back at 0.135.0, which never sees the gpt-5.6 models")
	}
	if !strings.Contains(CodexUserAgent, CodexClientVersion) {
		t.Errorf("user agent %q disagrees with client version %q", CodexUserAgent, CodexClientVersion)
	}
	if !strings.Contains(codexModelsURL, "client_version="+CodexClientVersion) {
		t.Errorf("models URL %q does not ask for client version %q", codexModelsURL, CodexClientVersion)
	}
}

// Verbatim shape of a /codex/models response, trimmed to the fields we read.
func TestParseCodexModelsSkipsHidden(t *testing.T) {
	body := []byte(`{"models":[
		{"slug":"gpt-5.6-sol","display_name":"GPT-5.6-Sol","visibility":"list"},
		{"slug":"gpt-reserve","display_name":"GPT-Reserve","visibility":"hide"},
		{"slug":"gpt-5.5","display_name":"GPT-5.5","visibility":"list"},
		{"slug":"codex-auto-review","display_name":"Codex Auto Review","visibility":"hide"}
	]}`)
	models, err := parseCodexModels(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := strings.Join(slugsOf(models), ",")
	if want := "gpt-5.6-sol,gpt-5.5"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseCodexModelsReadsCategories(t *testing.T) {
	body := []byte(`{"categories":[
		{"models":[{"slug":"gpt-5.6-sol","visibility":"list"},{"slug":"gpt-reserve","visibility":"hide"}]},
		{"models":[{"slug":"gpt-5.5","visibility":"list"}]}
	]}`)
	models, err := parseCodexModels(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := strings.Join(slugsOf(models), ",")
	if want := "gpt-5.6-sol,gpt-5.5"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A model without the field predates it upstream, so it stays listed.
func TestParseCodexModelsKeepsUnmarkedModels(t *testing.T) {
	models, err := parseCodexModels([]byte(`{"models":[{"slug":"gpt-5.5"}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(models) != 1 || models[0].Slug != "gpt-5.5" {
		t.Errorf("got %v, want [gpt-5.5]", slugsOf(models))
	}
}

func slugsOf(in []ModelInfo) []string {
	out := make([]string, len(in))
	for i, m := range in {
		out[i] = m.Slug
	}
	return out
}
