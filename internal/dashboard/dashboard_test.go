package dashboard

import (
	"strings"
	"testing"
)

func TestChatThinkingStatusUsesDedicatedAccessibleMarkup(t *testing.T) {
	index, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read embedded index: %v", err)
	}
	html := string(index)
	for _, want := range []string{
		`id="chat-status"`,
		`role="status"`,
		`aria-live="polite"`,
		`id="chat-status-model"`,
		`id="chat-status-elapsed"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("chat thinking status missing %q", want)
		}
	}

	css, err := staticFiles.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("read embedded styles: %v", err)
	}
	styles := string(css)
	if strings.Contains(styles, ".chat-output.loading::after") {
		t.Error("chat thinking status still uses the legacy appended pseudo-element")
	}
	if !strings.Contains(styles, "prefers-reduced-motion:reduce") {
		t.Error("chat thinking animation must respect reduced-motion preferences")
	}
}
