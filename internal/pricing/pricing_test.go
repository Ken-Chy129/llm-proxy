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
	tbl := New()
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
	tbl := New()
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
	tbl := New()
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

func TestCostSumsEveryBucket(t *testing.T) {
	tbl := New()
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

func TestNilTableIsSafe(t *testing.T) {
	var tbl *Table
	if _, ok := tbl.Lookup("claude-opus-4-6"); ok {
		t.Error("nil table must not price anything")
	}
	if tbl.All() != nil {
		t.Error("nil table must have no entries")
	}
}
