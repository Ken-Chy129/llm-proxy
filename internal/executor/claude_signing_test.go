package executor

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGenerateBillingHeaderMatchesClaudeCodeCustomBaseAttribution(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"Reply with exactly OK"}]}`)

	got := generateBillingHeader(body, "2.1.220")
	want := "x-anthropic-billing-header: cc_version=2.1.220.032; cc_entrypoint=sdk-cli;"
	if got != want {
		t.Fatalf("billing header=%q want %q", got, want)
	}
	if strings.Contains(got, "cch=") {
		t.Fatalf("custom-base attribution must not synthesize first-party cch: %q", got)
	}
}

func TestInjectClaudeCodeSystemBlocksDoesNotMutateExistingBillingHeader(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hello"}],"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220.abc; cc_entrypoint=sdk-cli; cch=00000;"},{"type":"text","text":"existing"}]}`)

	got := injectClaudeCodeSystemBlocks(body)
	if string(got) != string(body) {
		t.Fatalf("existing client attribution was mutated:\n got: %s\nwant: %s", got, body)
	}
}

func TestInjectClaudeCodeSystemBlocksUsesRequestFingerprintWithoutCCH(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"Reply with exactly OK"}],"system":"Hermes system prompt"}`)

	got := injectClaudeCodeSystemBlocks(body)
	var parsed struct {
		System []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("parse injected body: %v", err)
	}
	if len(parsed.System) != 3 {
		t.Fatalf("system blocks=%d want 3: %s", len(parsed.System), got)
	}
	wantHeader := "x-anthropic-billing-header: cc_version=2.1.220.032; cc_entrypoint=sdk-cli;"
	if parsed.System[0].Text != wantHeader {
		t.Fatalf("billing header=%q want %q", parsed.System[0].Text, wantHeader)
	}
	if strings.Contains(parsed.System[0].Text, "cch=") {
		t.Fatalf("injected billing header contains first-party cch: %q", parsed.System[0].Text)
	}
	if parsed.System[2].Text != "Hermes system prompt" {
		t.Fatalf("original system prompt not preserved: %#v", parsed.System)
	}
}
