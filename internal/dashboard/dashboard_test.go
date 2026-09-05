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

func TestStatusRefreshPreservesProviderCardInteractionState(t *testing.T) {
	app, err := staticFiles.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read embedded app: %v", err)
	}
	script := string(app)

	// Opening a <details> mutates the live DOM by adding its `open` attribute.
	// That transient browser state must not make an otherwise unchanged provider
	// card look stale, and it must survive a real server-side card update too.
	if strings.Contains(script, "} else if (!node.isEqualNode(next)) {") {
		t.Error("keyed refresh still compares live DOM, so opening View list replaces the provider card")
	}
	for _, want := range []string{
		"const refreshMarkup = new WeakMap();",
		"function captureRefreshState(node)",
		"function restoreRefreshState(node, state)",
		"refreshMarkup.get(node) !== html",
		"details: Array.from(node.querySelectorAll('details')).map",
		"scroll: Array.from(node.querySelectorAll('[data-refresh-scroll]')).map",
		"restoreRefreshState(node, state)",
		`data-refresh-scroll`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("status refresh does not preserve provider interaction state: missing %q", want)
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
		"anygen: 'AnyGen'",
		"modelBackends.get(model) !== 'anygen'",
		"function renderBackendModels(models, catalog, provider)",
		"backend-models-collapsible",
		"q.kind === 'credits'",
		"quota-credit-value",
		"choices?.[0]?.message?.content",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("AnyGen dashboard integration missing %q", want)
		}
	}

	css, err := staticFiles.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("read embedded styles: %v", err)
	}
	styles := string(css)
	for _, want := range []string{
		".backend-models-collapsible",
		".backend-model-list{",
		".quota-credit-value{",
	} {
		if !strings.Contains(styles, want) {
			t.Errorf("AnyGen compact/quota styles missing %q", want)
		}
	}
}

// A provider card that lists only the routed models cannot tell an operator
// which models are available to add — the case this covers. The card reads the
// discovered catalog alongside the routing table and marks what is unrouted.
func TestProviderCardSurfacesUnroutedUpstreamModels(t *testing.T) {
	app, err := staticFiles.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read embedded app: %v", err)
	}
	script := string(app)
	for _, want := range []string{
		"renderBackendModels(b.models, b.catalog, b.name)",
		"catalog?.models || []",
		"is-unrouted",
		"available to add",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("provider card catalog rendering missing %q", want)
		}
	}
	// The card is about the upstream, so a discovered catalog is drawn as-is.
	// Merging routed-but-undiscovered names back in put routing-table artifacts
	// (aliases, chains naming ids the upstream lacks) on a card that claims to
	// describe the provider.
	for _, gone := range []string{
		"routed.filter(m => !known.has(m))",
		"[...extra, ...entries]",
	} {
		if strings.Contains(script, gone) {
			t.Errorf("provider card still merges routing-table names into the catalog: %q", gone)
		}
	}

	css, err := staticFiles.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("read embedded styles: %v", err)
	}
	styles := string(css)
	for _, want := range []string{".model-tag.is-unrouted{", ".model-tag-publish{"} {
		if !strings.Contains(styles, want) {
			t.Errorf("catalog styles missing %q", want)
		}
	}
}

// Noticing a new upstream model is only useful if acting on it is one click, so
// the unrouted chips carry a publish control wired to the admin API.
func TestUnroutedModelsCanBePublishedFromTheProviderCard(t *testing.T) {
	app, err := staticFiles.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read embedded app: %v", err)
	}
	script := string(app)
	for _, want := range []string{
		"async function publishModel(provider, model, event)",
		"'/api/publish'",
		"model-tag-publish",
		// The chip lives inside a <summary>; without this the click also
		// collapses the list and the row acted on vanishes.
		"event.stopPropagation()",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("publish control missing %q", want)
		}
	}

	css, err := staticFiles.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("read embedded styles: %v", err)
	}
	if !strings.Contains(string(css), ".model-tag-publish{") {
		t.Error("publish control has no styling")
	}
}

// The upstream-rename box used to be free text, so a typo only surfaced when a
// real request failed. It now suggests the provider's discovered catalog and
// flags an id that catalog never listed — while still accepting one, because
// Vertex has no discovery endpoint and catalogs can lag a new model.
func TestUpstreamRenameBoxIsGuidedByTheProviderCatalog(t *testing.T) {
	app, err := staticFiles.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read embedded app: %v", err)
	}
	script := string(app)
	for _, want := range []string{
		"createElement('datalist')",
		"inp.setAttribute('list', listID)",
		"meta.catalog || []",
		// A blank box sends the published name, so that is the id to check.
		"const effective = typed || opts.published || '';",
		"map.classList.toggle('is-unknown', unknown)",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("catalog-guided rename box missing %q", want)
		}
	}
	// A <select> here would make the catalog a rule rather than a hint and lock
	// out every id the provider serves without advertising.
	if strings.Contains(script, "inp = document.createElement('select')") {
		t.Error("rename box must stay an input: the catalog is a hint, not a whitelist")
	}

	css, err := staticFiles.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("read embedded styles: %v", err)
	}
	if !strings.Contains(string(css), ".mdl-map.is-unknown") {
		t.Error("unknown-upstream warning has no styling")
	}
}

// The config page's subject is the published model and its ordered providers.
// These assertions pin the contract it has with the admin API — the field names
// the editor reads and writes — because a rename on either side fails silently
// in a browser and nowhere else.
func TestModelRoutingEditorIsWiredToTheConfigAPI(t *testing.T) {
	index, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read embedded index: %v", err)
	}
	html := string(index)
	for _, want := range []string{
		`id="cfg-models"`,
		`id="cfg-series"`,
		`id="save-models"`,
		`id="save-series"`,
		`onclick="saveSeries(this)"`,
		`onclick="addModelFromInput()"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("routing editor markup missing %q", want)
		}
	}
	// The old backend-partitioned editor and its price inputs are gone; prices
	// are published rates now, not a per-deployment setting.
	for _, gone := range []string{`id="cfg-routing-priority"`, `id="save-routing"`} {
		if strings.Contains(html, gone) {
			t.Errorf("markup still carries the removed backend-priority editor: %q", gone)
		}
	}

	app, err := staticFiles.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read embedded app: %v", err)
	}
	script := string(app)
	for _, want := range []string{
		"function renderModelCard(",
		"function renderChain(",
		"function renderSeriesDefaults()",
		"async function saveModels(btn)",
		"async function saveSeries(btn)",
		"async function pinModel(model, provider)",
		"'/api/pin'",
		"cfgProviders = d.providers || []",
		"aria-label",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("routing editor behavior missing %q", want)
		}
	}
	// Pricing is read-only now: an editor that writes prices back would be
	// saving a table the server no longer accepts.
	for _, gone := range []string{"priceOverrides", "backend_priority", "togglePriceEditor"} {
		if strings.Contains(script, gone) {
			t.Errorf("app.js still references removed machinery %q", gone)
		}
	}

	css, err := staticFiles.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("read embedded styles: %v", err)
	}
	styles := string(css)
	for _, want := range []string{".mdl-model{", ".mdl-chain{", ".mdl-pin{"} {
		if !strings.Contains(styles, want) {
			t.Errorf("routing editor styles missing %q", want)
		}
	}
}
