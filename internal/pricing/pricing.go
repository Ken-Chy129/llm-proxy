// Package pricing turns a token breakdown into a dollar figure.
//
// The number it produces is *list API cost*: what the same request would have
// cost on the provider's pay-per-token API. For Vertex and Kimi that is close to
// an actual invoice. For the OAuth backends (Claude Code / Codex subscriptions)
// there is no per-token bill at all — the marginal cost of those requests is
// zero and the figure is the value you got out of the subscription, not money
// spent. The dashboard labels it accordingly.
package pricing

import (
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

// Price is USD per 1M tokens, per bucket. The buckets line up 1:1 with
// types.TokenUsage, so cost is a dot product and nothing has to know which
// upstream convention the counts came from.
//
// CacheRead/CacheWrite are absolute prices, not multipliers — OpenAI charges
// nothing to write a cache entry, Anthropic charges 1.25x input, and expressing
// both as "a price" is the only form that fits both.
type Price struct {
	Name       string  `yaml:"name" json:"name"`
	Input      float64 `yaml:"input" json:"input"`
	Output     float64 `yaml:"output" json:"output"`
	CacheRead  float64 `yaml:"cache_read" json:"cache_read"`
	CacheWrite float64 `yaml:"cache_write" json:"cache_write"`
}

// Cost values the four buckets. Reasoning is deliberately ignored: it is a
// subset of Output and is already billed inside it.
func (p Price) Cost(u types.TokenUsage) float64 {
	return (float64(u.Input)*p.Input +
		float64(u.CacheRead)*p.CacheRead +
		float64(u.CacheWrite)*p.CacheWrite +
		float64(u.Output)*p.Output) / 1e6
}

// anthropic fills in Anthropic's published cache multipliers: a cache read
// costs 0.1x the input price, a 5-minute cache write 1.25x. We only ever see
// one cache_creation figure on the wire, so the 1h TTL (2x) is not modelled —
// a 1h-cached workload is undercounted, which is stated in the docs rather than
// guessed at here.
func anthropic(in, out float64) Price {
	return Price{Input: in, Output: out, CacheRead: in * 0.1, CacheWrite: in * 1.25}
}

// openai models charge a discounted rate on cached input and nothing to write,
// so CacheWrite stays 0.
func openai(in, cached, out float64) Price {
	return Price{Input: in, Output: out, CacheRead: cached}
}

// builtin is the default price table, in USD per 1M tokens.
//
// Sources (checked 2026-08-05): Anthropic list pricing via the claude-api
// reference; OpenAI via developers.openai.com/api/docs/pricing; Moonshot via
// kimi.com/help/kimi-api/api-pricing. These are first-party list rates —
// Vertex and Bedrock bill Claude separately and can differ, and long-context
// tiers (OpenAI >272K) and batch/flex discounts are not modelled.
//
// A model is priced by what it *is*, not by which provider served it: the same
// claude-opus-5 costs the same whether it arrived over an OAuth subscription or
// a metered relay, so one rate per model is the whole story.
var builtin = map[string]Price{
	// --- Anthropic ---
	"claude-fable-5":    anthropic(10, 50),
	"claude-mythos-5":   anthropic(10, 50),
	"claude-opus-5":     anthropic(5, 25),
	"claude-opus-4-8":   anthropic(5, 25),
	"claude-opus-4-7":   anthropic(5, 25),
	"claude-opus-4-6":   anthropic(5, 25),
	"claude-opus-4-5":   anthropic(5, 25),
	"claude-opus-4-1":   anthropic(15, 75),
	"claude-opus-4":     anthropic(15, 75),
	"claude-sonnet-5":   anthropic(3, 15),
	"claude-sonnet-4-6": anthropic(3, 15),
	"claude-sonnet-4-5": anthropic(3, 15),
	"claude-sonnet-4":   anthropic(3, 15),
	"claude-haiku-4-5":  anthropic(1, 5),
	"claude-3-7-sonnet": anthropic(3, 15),
	"claude-3-5-sonnet": anthropic(3, 15),
	"claude-3-5-haiku":  anthropic(0.8, 4),
	"claude-3-haiku":    anthropic(0.25, 1.25),
	"claude-3-opus":     anthropic(15, 75),

	// --- OpenAI (Codex) ---
	// gpt-5.6 is the one family that also charges for cache writes.
	"gpt-5.6-sol":   {Input: 5, Output: 30, CacheRead: 0.5, CacheWrite: 6.25},
	"gpt-5.6-terra": {Input: 2, Output: 12, CacheRead: 0.2, CacheWrite: 2.5},
	"gpt-5.6-luna":  {Input: 0.2, Output: 1.2, CacheRead: 0.02, CacheWrite: 0.25},
	"gpt-5.5-pro":   openai(30, 30, 180),
	"gpt-5.5":       openai(5, 0.5, 30),
	"gpt-5.4-pro":   openai(30, 30, 180),
	"gpt-5.4-mini":  openai(0.75, 0.075, 4.5),
	"gpt-5.4-nano":  openai(0.2, 0.02, 1.25),
	"gpt-5.4":       openai(2.5, 0.25, 15),
	"gpt-5.3-codex": openai(1.75, 0.175, 14),
	"gpt-5.2-pro":   openai(21, 21, 168),
	"gpt-5.2":       openai(1.75, 0.175, 14),
	"gpt-5.1":       openai(1.25, 0.125, 10),
	"gpt-5-pro":     openai(15, 15, 120),
	"gpt-5-mini":    openai(0.25, 0.025, 2),
	"gpt-5-nano":    openai(0.05, 0.005, 0.4),
	"gpt-5":         openai(1.25, 0.125, 10),

	// --- Moonshot / Kimi ---
	// The kimi-for-coding* plans are subscription seats with no per-token rate,
	// so they are deliberately absent: unpriced beats wrong.
	"kimi-k3":   openai(3, 0.3, 15),
	"kimi-k2.7": openai(0.95, 0.19, 4),
	"kimi-k2.6": openai(0.95, 0.16, 4),
	"kimi-k2.5": openai(0.6, 0.1, 3),
}

// minPrefixLen guards prefix matching. Exact lookups are always tried first;
// prefix matching then lets "claude-sonnet-4-6-oauth-1m" fall back to its base
// model, but a two-character key like "k3" matching everything starting with k3
// would do more harm than good.
const minPrefixLen = 6

// dateSuffix matches the dated-snapshot form both providers use:
// claude-haiku-4-5-20251001, claude-opus-4-5@20251101.
var dateSuffix = regexp.MustCompile(`[-@]\d{8}$`)

// normalize maps whatever name the request carried onto a table key. Model names
// reaching us are the names this proxy publishes, which are already the model's
// canonical name. Normalising still earns its keep for dated snapshots and the
// fully-qualified ids that appear in logs from Vertex and Bedrock.
func normalize(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	// publishers/anthropic/models/claude-opus-4-6 → claude-opus-4-6
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	m = strings.TrimPrefix(m, "anthropic.")
	m = dateSuffix.ReplaceAllString(m, "")
	return m
}

// Table is a price lookup, safe for concurrent use.
type Table struct {
	mu       sync.RWMutex
	entries  map[string]Price
	prefixes []string // keys eligible for prefix matching, longest first
}

// New builds the price table. Rates are a published property of each model, so
// there is nothing to configure: the table is the same on every deployment.
func New() *Table {
	t := &Table{entries: make(map[string]Price, len(builtin))}
	for name, p := range builtin {
		p.Name = name
		t.entries[name] = p
	}
	t.reindex()
	return t
}

// reindex must be called with t.mu held, or before the table is shared.
func (t *Table) reindex() {
	t.prefixes = t.prefixes[:0]
	for name := range t.entries {
		if len(name) >= minPrefixLen {
			t.prefixes = append(t.prefixes, name)
		}
	}
	// Longest first so gpt-5.4-mini wins over gpt-5.4, and claude-opus-4-6 wins
	// over any shorter key that happens to share its head.
	sort.Slice(t.prefixes, func(i, j int) bool {
		if len(t.prefixes[i]) != len(t.prefixes[j]) {
			return len(t.prefixes[i]) > len(t.prefixes[j])
		}
		return t.prefixes[i] < t.prefixes[j]
	})
}

// Lookup resolves a model name to its price. The second return distinguishes
// "priced at zero" from "we have no idea what this costs" — every caller that
// displays a figure needs to tell those apart.
func (t *Table) Lookup(model string) (Price, bool) {
	if t == nil {
		return Price{}, false
	}
	name := normalize(model)
	if name == "" {
		return Price{}, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lookupLocked(name)
}

// lookupLocked must be called with t.mu held (read is enough).
func (t *Table) lookupLocked(name string) (Price, bool) {
	if p, ok := t.entries[name]; ok {
		return p, true
	}
	for _, k := range t.prefixes {
		if strings.HasPrefix(name, k) {
			return t.entries[k], true
		}
	}
	return Price{}, false
}

// Cost values a usage breakdown for a model. ok is false when the model is
// unpriced, in which case callers should record "unknown" rather than 0.
func (t *Table) Cost(model string, u types.TokenUsage) (cost float64, ok bool) {
	p, ok := t.Lookup(model)
	if !ok {
		return 0, false
	}
	return p.Cost(u), true
}

// All returns the effective table sorted by model name, for display.
func (t *Table) All() []Price {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Price, 0, len(t.entries))
	for _, p := range t.entries {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
