package pricing

import (
	"math"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

func approx(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// The names that actually reach Record are proxy aliases, not upstream IDs:
// config.yaml invents them, Vertex prefixes them, and OAuth models carry this
// project's own "-oauth" suffix. All of those must price like the base model.
func TestLookupNormalisesModelNames(t *testing.T) {
	tbl := New(nil)
	for _, name := range []string{
		"claude-opus-4-6",
		"claude-opus-4-6-oauth",
		"Claude-Opus-4-6",
		"anthropic.claude-opus-4-6",
		"publishers/anthropic/models/claude-opus-4-6",
		"claude-opus-4-6@20260101",
		"claude-opus-4-6-20260101",
	} {
		p, ok := tbl.Lookup(name)
		if !ok {
			t.Fatalf("%q: not priced", name)
		}
		if p.Input != 5 || p.Output != 25 {
			t.Errorf("%q priced at %v/%v, want 5/25", name, p.Input, p.Output)
		}
	}
}

// Longest-prefix matching, not first-match: gpt-5.4-mini shares its head with
// gpt-5.4 and is six times cheaper.
func TestLookupPrefersLongestPrefix(t *testing.T) {
	tbl := New(nil)
	mini, ok := tbl.Lookup("gpt-5.4-mini")
	if !ok {
		t.Fatal("gpt-5.4-mini not priced")
	}
	approx(t, mini.Input, 0.75, "gpt-5.4-mini input")

	// An unknown variant still falls back to its base model rather than nothing.
	fast, ok := tbl.Lookup("gpt-5.4-something-new")
	if !ok {
		t.Fatal("gpt-5.4 variant did not fall back")
	}
	approx(t, fast.Input, 2.5, "gpt-5.4 variant input")
}

// An unpriced model must report unknown, never $0 — a subscription seat and a
// model nobody has priced look identical if this collapses.
func TestLookupUnknownModel(t *testing.T) {
	tbl := New(nil)
	if _, ok := tbl.Lookup("kimi-for-coding"); ok {
		t.Error("kimi-for-coding is a subscription seat and must stay unpriced")
	}
	if _, ok := tbl.Lookup(""); ok {
		t.Error("empty model name must not resolve")
	}
	if cost, ok := tbl.Cost("who-knows", types.TokenUsage{Input: 1000}); ok || cost != 0 {
		t.Errorf("unknown model = (%v, %v), want (0, false)", cost, ok)
	}
}

func TestOverridesReplaceAndExtend(t *testing.T) {
	tbl := New([]Price{
		{Name: "claude-opus-4-6", Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4},
		{Name: "my-local-llama", Input: 0.1, Output: 0.2},
	})

	p, ok := tbl.Lookup("claude-opus-4-6-oauth")
	if !ok {
		t.Fatal("override lost the model")
	}
	if p.Input != 1 || p.Output != 2 || p.CacheRead != 3 || p.CacheWrite != 4 {
		t.Errorf("override not applied: %+v", p)
	}

	if _, ok := tbl.Lookup("my-local-llama"); !ok {
		t.Error("override did not extend the table")
	}
}

// An all-zeros override is the documented way to say "this one really is free",
// which is a different claim from "unpriced".
func TestZeroOverrideIsKnownFree(t *testing.T) {
	tbl := New([]Price{{Name: "kimi-for-coding"}})
	cost, ok := tbl.Cost("kimi-for-coding", types.TokenUsage{Input: 1e6, Output: 1e6})
	if !ok {
		t.Fatal("zero-price override must count as priced")
	}
	approx(t, cost, 0, "free model cost")
}

func TestCostSumsEveryBucket(t *testing.T) {
	tbl := New(nil)
	// Opus-tier: $5 input, $25 output, so $0.50 read and $6.25 write.
	cost, ok := tbl.Cost("claude-opus-4-6", types.TokenUsage{
		Input:      1_000_000,
		CacheRead:  1_000_000,
		CacheWrite: 1_000_000,
		Output:     1_000_000,
		Reasoning:  500_000, // subset of Output; must not be billed twice
	})
	if !ok {
		t.Fatal("not priced")
	}
	approx(t, cost, 5+0.5+6.25+25, "opus cost")
}

// An alias that looks nothing like its upstream model is the case that silently
// loses money from the totals, because the alias is what gets recorded.
func TestAliasFallback(t *testing.T) {
	tbl := New(nil)
	if _, ok := tbl.Lookup("sonnet"); ok {
		t.Fatal("precondition: 'sonnet' should be unpriced without an alias map")
	}

	tbl.SetAliases(map[string]string{
		"sonnet":          "claude-sonnet-4-6",
		"kimi-for-coding": "kimi-for-coding", // self-mapping must not loop or resolve
	})

	p, ok := tbl.Lookup("sonnet")
	if !ok {
		t.Fatal("alias did not resolve to its upstream price")
	}
	approx(t, p.Input, 3, "aliased input price")
	if _, ok := tbl.Lookup("kimi-for-coding"); ok {
		t.Error("a self-mapping alias must not invent a price")
	}
}

// A direct override on the alias beats the upstream's list price: that is how
// "this alias is a free subscription seat" is expressed.
func TestAliasDoesNotOverrideDirectHit(t *testing.T) {
	tbl := New([]Price{{Name: "seat"}}) // all zeros = known free
	tbl.SetAliases(map[string]string{"seat": "claude-opus-4-6"})
	p, ok := tbl.Lookup("seat")
	if !ok {
		t.Fatal("override lost")
	}
	approx(t, p.Input, 0, "override must win over the alias target")
}

func TestNilTableIsSafe(t *testing.T) {
	var tbl *Table
	if _, ok := tbl.Lookup("claude-opus-4-6"); ok {
		t.Error("nil table must not price anything")
	}
	if tbl.All() != nil {
		t.Error("nil table must have no entries")
	}
}
