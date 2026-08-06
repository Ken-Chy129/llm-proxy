package stats

import (
	"math"
	"testing"
	"time"

	"github.com/Ken-Chy129/llm-proxy/internal/pricing"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
)

// testPrices is a deliberately round table so expected costs are readable:
// $1 in / $10 out / $0.10 read / $2 write per 1M tokens.
func testPrices() *pricing.Table {
	return pricing.New([]pricing.Price{
		{Name: "priced-model", Input: 1, Output: 10, CacheRead: 0.1, CacheWrite: 2},
	})
}

func record(t *testing.T, d *DB, model string, u types.TokenUsage) *RequestLog {
	t.Helper()
	l := &RequestLog{Time: time.Now(), Model: model, Backend: "test", Status: 200, APIKeyName: "k"}
	l.SetUsage(u)
	if err := d.Record(l); err != nil {
		t.Fatalf("record: %v", err)
	}
	return l
}

func approx(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestRecordPricesEveryBucket(t *testing.T) {
	db := newTestDB(t)
	db.SetPricing(testPrices())

	l := record(t, db, "priced-model", types.TokenUsage{
		Input: 1_000_000, CacheRead: 1_000_000, CacheWrite: 1_000_000, Output: 1_000_000,
		Reasoning: types.ReasoningUnknown,
	})
	if !l.CostKnown {
		t.Fatal("priced model recorded as unpriced")
	}
	approx(t, l.CostUSD, 1+0.1+2+10, "recorded cost")

	s := db.StatsSummary(1, "", "")
	approx(t, s.CostUSD, 13.1, "summary cost")
	if s.CostKnownRequests != 1 {
		t.Errorf("cost_known_requests = %d, want 1", s.CostKnownRequests)
	}
}

// An unpriced model must not silently contribute $0 to a total that is then
// presented as the whole bill — the row count is what lets the UI say so.
func TestUnpricedModelIsUnknownNotZero(t *testing.T) {
	db := newTestDB(t)
	db.SetPricing(testPrices())

	record(t, db, "priced-model", types.TokenUsage{Input: 1_000_000})
	l := record(t, db, "mystery-model", types.TokenUsage{Input: 5_000_000})
	if l.CostKnown {
		t.Error("unknown model must not be priced")
	}

	s := db.StatsSummary(1, "", "")
	approx(t, s.CostUSD, 1, "summary must exclude the unpriced request")
	if s.Requests != 2 || s.CostKnownRequests != 1 {
		t.Errorf("requests=%d cost_known=%d, want 2 and 1", s.Requests, s.CostKnownRequests)
	}

	logs, _, err := db.QueryLogs(10, 0, false, "")
	if err != nil {
		t.Fatalf("query logs: %v", err)
	}
	for _, row := range logs {
		if row.Model == "mystery-model" && (row.CostKnown || row.CostUSD != 0) {
			t.Errorf("unpriced row round-tripped as %v/%v", row.CostUSD, row.CostKnown)
		}
	}
}

// Rows written before costing existed (or while a model had no price) are
// valued once, on the next start with a table that covers them.
func TestSetPricingBackfillsHistory(t *testing.T) {
	db := newTestDB(t)

	// No pricing installed yet: this row lands with cost_usd NULL.
	record(t, db, "priced-model", types.TokenUsage{Input: 2_000_000, Output: 1_000_000})
	if s := db.StatsSummary(1, "", ""); s.CostKnownRequests != 0 {
		t.Fatalf("expected an unpriced row before SetPricing, got %d priced", s.CostKnownRequests)
	}

	db.SetPricing(testPrices())

	s := db.StatsSummary(1, "", "")
	approx(t, s.CostUSD, 2+10, "backfilled cost")
	if s.CostKnownRequests != 1 {
		t.Errorf("cost_known_requests = %d, want 1 after backfill", s.CostKnownRequests)
	}
}

// Costing must not change what the token accounting reports; the two are
// independent readings of the same row.
func TestCostFlowsIntoDimensionAndKeyStats(t *testing.T) {
	db := newTestDB(t)
	db.SetPricing(testPrices())
	record(t, db, "priced-model", types.TokenUsage{Input: 1_000_000, Output: 100_000})

	byModel, err := db.StatsByDimension("model", 1, "", "")
	if err != nil {
		t.Fatalf("by model: %v", err)
	}
	if len(byModel) != 1 {
		t.Fatalf("got %d model rows, want 1", len(byModel))
	}
	approx(t, byModel[0].CostUSD, 2, "per-model cost")
	if byModel[0].TotalTokens != 1_100_000 {
		t.Errorf("per-model tokens = %d, want 1100000", byModel[0].TotalTokens)
	}

	keys, err := db.StatsByKey(0)
	if err != nil {
		t.Fatalf("by key: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d key rows, want 1", len(keys))
	}
	approx(t, keys[0].CostUSD, 2, "per-key cost")
}
