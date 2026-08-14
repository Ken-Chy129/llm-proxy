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

func TestLogTableConstrainsLongErrorRows(t *testing.T) {
	index, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read embedded index: %v", err)
	}
	html := string(index)
	for _, want := range []string{
		`class="log-table-wrap"`,
		`class="log-table"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("log table markup missing %q", want)
		}
	}

	css, err := staticFiles.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("read embedded styles: %v", err)
	}
	styles := string(css)
	for _, want := range []string{
		`.log-table-wrap{overflow-x:auto;`,
		`.log-table{table-layout:fixed;`,
		`.log-err{display:block;`,
		`.log-controls{display:grid;grid-template-columns:1fr 1fr;width:100%;`,
		`.log-search{grid-column:1/-1;width:100%;`,
	} {
		if !strings.Contains(styles, want) {
			t.Errorf("log table styles missing %q", want)
		}
	}
}

func TestStatusRefreshPatchesRenderedListsInsteadOfReplacingThem(t *testing.T) {
	app, err := staticFiles.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read embedded app: %v", err)
	}
	script := string(app)

	// /api/status is polled while the page is visible. Replacing these containers'
	// innerHTML on every response destroys and recreates otherwise-identical DOM,
	// which causes visible layout jumps and loses transient element state.
	for _, forbidden := range []string{
		"oauthEl.innerHTML =",
		"apiEl.innerHTML =",
		"qGrid.innerHTML =",
		"sel.innerHTML =",
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("status refresh still replaces a rendered container with %q", forbidden)
		}
	}

	for _, want := range []string{
		"function syncKeyedHTML(",
		"syncKeyedHTML(oauthEl,",
		"syncKeyedHTML(apiEl,",
		"syncKeyedHTML(qGrid,",
		"syncHTML(sel,",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("status refresh is missing incremental DOM sync %q", want)
		}
	}
}

func TestAnyGenAppearsAsDynamicAPIBackend(t *testing.T) {
	app, err := staticFiles.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read embedded app: %v", err)
	}
	script := string(app)
	for _, want := range []string{
		"id: 'anygen', label: 'AnyGen'",
		"anygen: (d.anygen?.models || [])",
		"anygen: { models: names('anygen') }",
		"b.name === 'anygen'",
		"modelBackends.get(model) !== 'anygen'",
		"choices?.[0]?.message?.content",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("AnyGen dashboard integration missing %q", want)
		}
	}
}
