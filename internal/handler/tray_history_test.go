package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Ken-Chy129/llm-proxy/internal/stats"
	"github.com/Ken-Chy129/llm-proxy/internal/types"
	"github.com/gin-gonic/gin"
)

// TrayHistory only reads statsDB, so the other AdminHandler dependencies stay nil.
func newHistoryHandler(t *testing.T) (*AdminHandler, *stats.DB) {
	t.Helper()
	db, err := stats.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open stats db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &AdminHandler{statsDB: db}, db
}

func callHistory(t *testing.T, h *AdminHandler, query string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/tray/history"+query, nil)
	h.TrayHistory(c)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON (%d): %s", w.Code, w.Body.String())
	}
	return w.Code, body
}

func recordDay(t *testing.T, db *stats.DB, when time.Time, requests int, tokens types.TokenUsage) {
	t.Helper()
	for i := 0; i < requests; i++ {
		e := &stats.RequestLog{
			Time: when, Model: "claude-opus-5", Backend: "claude",
			LatencyMs: 1000, Status: 200, Stream: true, APIKeyName: "k",
		}
		e.SetUsage(tokens)
		if err := db.Record(e); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
}

// An empty database must serialise as [] rather than null: the widget iterates
// this directly, and null would make it special-case a case that never needs to
// exist.
func TestTrayHistoryEmptyDBReturnsEmptyArray(t *testing.T) {
	h, _ := newHistoryHandler(t)
	code, body := callHistory(t, h, "?tz=480")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	raw, ok := body["days"]
	if !ok {
		t.Fatal(`response has no "days" key`)
	}
	if raw == nil {
		t.Error(`"days" is null; want []`)
	}
	if days, ok := raw.([]any); !ok || len(days) != 0 {
		t.Errorf(`"days" = %v, want an empty array`, raw)
	}
}

func TestTrayHistoryClampsDays(t *testing.T) {
	h, _ := newHistoryHandler(t)
	for _, tc := range []struct {
		query string
		want  float64
	}{
		{"?days=1", trayHistoryMinDays},       // below the floor
		{"?days=0", trayHistoryMinDays},       // zero is not "no limit"
		{"?days=-30", trayHistoryMinDays},     // negative would make the window run forward
		{"?days=99999", trayHistoryMaxDays},   // an unbounded scan is a denial-of-service shape
		{"?days=abc", trayHistoryDefaultDays}, // unparseable falls back, not errors
		{"", trayHistoryDefaultDays},          // absent
		{"?days=90", 90},                      // in range, honoured
	} {
		_, body := callHistory(t, h, tc.query)
		if got := body["range_days"]; got != tc.want {
			t.Errorf("days%q → range_days = %v, want %v", tc.query, got, tc.want)
		}
	}
}

// The heatmap fills its grid by date key, so days must come back keyed by
// calendar date, ascending, and only for days that saw traffic.
func TestTrayHistoryReturnsSparseAscendingDays(t *testing.T) {
	h, db := newHistoryHandler(t)
	const tz = 480
	loc := time.FixedZone("test", tz*60)
	today := time.Now().In(loc)
	midday := time.Date(today.Year(), today.Month(), today.Day(), 12, 0, 0, 0, loc)

	// Today and three days ago; the two days between stay empty on purpose.
	recordDay(t, db, midday, 2, types.TokenUsage{
		Input: 100, CacheRead: 9000, CacheWrite: 500, Output: 400, Reasoning: types.ReasoningUnknown,
	})
	recordDay(t, db, midday.AddDate(0, 0, -3), 1, types.TokenUsage{
		Input: 50, CacheRead: 0, CacheWrite: 0, Output: 25, Reasoning: types.ReasoningUnknown,
	})

	_, body := callHistory(t, h, "?tz=480&days=30")
	days, _ := body["days"].([]any)
	if len(days) != 2 {
		t.Fatalf("got %d days, want 2 (empty days must be omitted, not zero-filled): %v", len(days), days)
	}

	first := days[0].(map[string]any)
	last := days[1].(map[string]any)
	if first["d"].(string) >= last["d"].(string) {
		t.Errorf("days not ascending: %v then %v", first["d"], last["d"])
	}

	// The newest day: 2 requests × (100+9000+500+400) tokens, cache = read+write.
	if got, want := last["r"], float64(2); got != want {
		t.Errorf("requests = %v, want %v", got, want)
	}
	if got, want := last["t"], float64(2*(100+9000+500+400)); got != want {
		t.Errorf("tokens = %v, want %v", got, want)
	}
	if got, want := last["c"], float64(2*(9000+500)); got != want {
		t.Errorf("cache = %v, want %v (read+write)", got, want)
	}
	// Cache must never exceed the total it is a part of.
	if last["c"].(float64) > last["t"].(float64) {
		t.Errorf("cache %v exceeds total %v", last["c"], last["t"])
	}
}

// A junk tz must not shift calendar-day boundaries somewhere absurd; it falls
// back to UTC, same rule as /api/tray.
func TestTrayHistoryClampsTimezone(t *testing.T) {
	h, _ := newHistoryHandler(t)
	for _, q := range []string{"?tz=99999", "?tz=-99999", "?tz=notanumber", ""} {
		if code, _ := callHistory(t, h, q); code != http.StatusOK {
			t.Errorf("tz%q → status %d, want 200", q, code)
		}
	}
}
