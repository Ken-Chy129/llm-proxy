package stats

import (
	"testing"
	"time"
)

// insertAt records a request at a specific UTC instant so calendar-day and
// timezone boundaries can be asserted deterministically.
func insertAt(t *testing.T, d *DB, when time.Time, key string, prompt, completion int) {
	t.Helper()
	_, err := d.db.Exec(`
		INSERT INTO request_logs (time, model, backend, latency_ms, status, prompt_tokens, completion_tokens, stream, api_key_name)
		VALUES (?, 'test-model', 'test', 100, 200, ?, ?, 0, ?)`,
		when.UTC().Format(time.RFC3339Nano), prompt, completion, key)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
}

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestDayUsageIsCalendarDayNotRolling24h pins the core reason this API exists:
// StatsSummary(days=1) is a rolling window that counts traffic from yesterday
// evening, whereas the tray must report the calendar day only.
func TestDayUsageIsCalendarDayNotRolling24h(t *testing.T) {
	db := newTestDB(t)
	const tz = 480 // UTC+8

	loc := time.FixedZone("test", tz*60)
	nowLocal := time.Now().In(loc)
	todayLocal := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, loc)

	// 2 hours into today, local time — must be counted.
	insertAt(t, db, todayLocal.Add(2*time.Hour), "k-today", 100, 50)
	// 2 hours before today started (i.e. yesterday 22:00 local) — inside a
	// rolling 24h window, but must NOT be counted as today.
	insertAt(t, db, todayLocal.Add(-2*time.Hour), "k-yday", 700, 300)

	today, err := db.DayUsage(0, tz)
	if err != nil {
		t.Fatalf("DayUsage today: %v", err)
	}
	if today.RequestCount != 1 {
		t.Errorf("today requests = %d, want 1 (rolling-window bleed?)", today.RequestCount)
	}
	if today.TotalTokens != 150 {
		t.Errorf("today tokens = %d, want 150", today.TotalTokens)
	}

	yday, err := db.DayUsage(1, tz)
	if err != nil {
		t.Fatalf("DayUsage yesterday: %v", err)
	}
	if yday.RequestCount != 1 || yday.TotalTokens != 1000 {
		t.Errorf("yesterday = %d reqs / %d tokens, want 1/1000", yday.RequestCount, yday.TotalTokens)
	}

	// Sanity: the rolling summary sees both, which is exactly the discrepancy
	// the tray endpoint avoids.
	if reqs, _, _, _ := db.StatsSummary(1, "", ""); reqs != 2 {
		t.Errorf("StatsSummary(1) = %d, want 2 — test fixture no longer demonstrates the difference", reqs)
	}
}

// TestDayUsageTimezoneShiftsBoundary verifies a request near UTC midnight lands
// on different calendar days depending on the viewer's offset.
func TestDayUsageTimezoneShiftsBoundary(t *testing.T) {
	db := newTestDB(t)

	// 17:00 UTC today = 01:00 tomorrow in UTC+8, so for a UTC+8 viewer this
	// belongs to "tomorrow" and must be absent from today's figures. Use a
	// timestamp built from the UTC+8 day boundary to stay deterministic.
	loc8 := time.FixedZone("p8", 8*3600)
	nowIn8 := time.Now().In(loc8)
	today8 := time.Date(nowIn8.Year(), nowIn8.Month(), nowIn8.Day(), 0, 0, 0, 0, loc8)

	// 30 minutes into today for UTC+8 == 16:30 UTC *yesterday*.
	edge := today8.Add(30 * time.Minute)
	insertAt(t, db, edge, "k-edge", 10, 10)

	in8, err := db.DayUsage(0, 480)
	if err != nil {
		t.Fatalf("DayUsage +8: %v", err)
	}
	if in8.RequestCount != 1 {
		t.Errorf("UTC+8 today = %d requests, want 1", in8.RequestCount)
	}

	// The same row for a UTC viewer falls on the previous calendar day.
	inUTC, err := db.DayUsage(0, 0)
	if err != nil {
		t.Fatalf("DayUsage UTC: %v", err)
	}
	if inUTC.RequestCount != 0 {
		t.Errorf("UTC today = %d requests, want 0 (edge row belongs to prior UTC day)", inUTC.RequestCount)
	}
}

func TestDayUsageByKeyOrderedByTokens(t *testing.T) {
	db := newTestDB(t)
	const tz = 480
	loc := time.FixedZone("test", tz*60)
	nowLocal := time.Now().In(loc)
	base := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 3, 0, 0, 0, loc)

	insertAt(t, db, base, "small", 10, 10)
	insertAt(t, db, base.Add(time.Minute), "big", 5000, 5000)
	insertAt(t, db, base.Add(2*time.Minute), "big", 1000, 1000)
	insertAt(t, db, base.Add(3*time.Minute), "", 999, 999) // unattributed, must be excluded

	today, err := db.DayUsage(0, tz)
	if err != nil {
		t.Fatalf("DayUsage: %v", err)
	}
	if len(today.ByKey) != 2 {
		t.Fatalf("by_key len = %d, want 2 (empty key name must be filtered)", len(today.ByKey))
	}
	if today.ByKey[0].KeyName != "big" {
		t.Errorf("by_key[0] = %q, want \"big\" (descending token order)", today.ByKey[0].KeyName)
	}
	if today.ByKey[0].TotalTokens != 12000 {
		t.Errorf("big tokens = %d, want 12000", today.ByKey[0].TotalTokens)
	}
	if today.ByKey[0].RequestCount != 2 {
		t.Errorf("big requests = %d, want 2", today.ByKey[0].RequestCount)
	}
	// The unattributed row still counts toward the day total.
	if today.RequestCount != 4 {
		t.Errorf("day requests = %d, want 4", today.RequestCount)
	}
}

// TestDailyAverageExcludesToday guards the baseline: including a partial today
// would understate the average and make the tray's comparison misleading.
func TestDailyAverageExcludesToday(t *testing.T) {
	db := newTestDB(t)
	const tz = 480
	loc := time.FixedZone("test", tz*60)
	nowLocal := time.Now().In(loc)
	today := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, loc)

	// 700 tokens/day across yesterday and the day before.
	insertAt(t, db, today.Add(-20*time.Hour), "k", 350, 350) // yesterday
	insertAt(t, db, today.Add(-44*time.Hour), "k", 350, 350) // two days ago
	// A huge spike today that must be excluded from the baseline.
	insertAt(t, db, today.Add(time.Hour), "k", 500000, 500000)

	_, avgTokens, err := db.DailyAverage(7, tz)
	if err != nil {
		t.Fatalf("DailyAverage: %v", err)
	}
	want := 1400.0 / 7.0
	if avgTokens < want-0.001 || avgTokens > want+0.001 {
		t.Errorf("avg tokens = %f, want %f (today's spike must be excluded)", avgTokens, want)
	}

	avgReqs, _, _ := db.DailyAverage(7, tz)
	if want := 2.0 / 7.0; avgReqs < want-0.001 || avgReqs > want+0.001 {
		t.Errorf("avg requests = %f, want %f", avgReqs, want)
	}
}

func TestDailyAverageZeroDays(t *testing.T) {
	db := newTestDB(t)
	r, tk, err := db.DailyAverage(0, 480)
	if err != nil {
		t.Fatalf("DailyAverage(0): %v", err)
	}
	if r != 0 || tk != 0 {
		t.Errorf("got %f/%f, want 0/0", r, tk)
	}
}

func TestHourlyTodayBucketsByLocalHour(t *testing.T) {
	db := newTestDB(t)
	const tz = 480
	loc := time.FixedZone("test", tz*60)
	nowLocal := time.Now().In(loc)
	today := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, loc)

	insertAt(t, db, today.Add(9*time.Hour+15*time.Minute), "k", 100, 100)
	insertAt(t, db, today.Add(9*time.Hour+45*time.Minute), "k", 50, 50)
	insertAt(t, db, today.Add(23*time.Hour), "k", 7, 3)

	buckets, err := db.HourlyToday(tz)
	if err != nil {
		t.Fatalf("HourlyToday: %v", err)
	}
	if len(buckets) != 24 {
		t.Fatalf("len = %d, want 24 fixed slots", len(buckets))
	}
	if buckets[9] != 300 {
		t.Errorf("hour 9 = %d, want 300", buckets[9])
	}
	if buckets[23] != 10 {
		t.Errorf("hour 23 = %d, want 10", buckets[23])
	}
	if buckets[0] != 0 {
		t.Errorf("hour 0 = %d, want 0", buckets[0])
	}
}

func TestTzOffsetClampsOutOfRange(t *testing.T) {
	for _, tc := range []struct{ in int; want string }{
		{480, ", '+480 minutes'"},
		{-300, ", '-300 minutes'"},
		{9999, ", '+0 minutes'"},  // clamped
		{-9999, ", '+0 minutes'"}, // clamped
	} {
		if got := tzOffset(tc.in); got != tc.want {
			t.Errorf("tzOffset(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLastRequestTimeEmptyDB(t *testing.T) {
	db := newTestDB(t)
	if _, ok := db.LastRequestTime(); ok {
		t.Error("expected ok=false on empty table")
	}
}

func TestLastRequestTimeReturnsNewest(t *testing.T) {
	db := newTestDB(t)
	newest := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
	insertAt(t, db, newest.Add(-2*time.Hour), "k", 1, 1)
	insertAt(t, db, newest, "k", 1, 1)

	got, ok := db.LastRequestTime()
	if !ok {
		t.Fatal("expected ok=true")
	}
	if diff := got.UTC().Sub(newest); diff > time.Second || diff < -time.Second {
		t.Errorf("got %v, want ~%v", got.UTC(), newest)
	}
}
