package stats

import (
	"fmt"
	"time"
)

// TrayKeyUsage is one API key's usage inside a calendar-day window.
type TrayKeyUsage struct {
	KeyName      string  `json:"key_name"`
	RequestCount int     `json:"request_count"`
	TotalTokens  int     `json:"total_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// TrayDayUsage aggregates one calendar day of traffic in the viewer's timezone.
type TrayDayUsage struct {
	Date         string         `json:"date"`
	RequestCount int            `json:"request_count"`
	TotalTokens  int            `json:"total_tokens"`
	ByKey        []TrayKeyUsage `json:"by_key,omitempty"`
	TokenBreakdown
}

// tzOffset renders a tzMinutes offset as a SQLite datetime modifier, clamped to
// the real-world range so it can be interpolated into SQL safely.
func tzOffset(tzMinutes int) string {
	if tzMinutes < -720 || tzMinutes > 840 {
		tzMinutes = 0
	}
	return fmt.Sprintf(", '%+d minutes'", tzMinutes)
}

// DayUsage returns traffic for a single calendar day in the viewer's timezone.
//
// This differs from StatsSummary(days=1), which is a rolling 24-hour window and
// therefore bleeds yesterday's traffic into "today". dayOffset counts backwards
// from today: 0 = today, 1 = yesterday.
func (d *DB) DayUsage(dayOffset, tzMinutes int) (TrayDayUsage, error) {
	tzMod := tzOffset(tzMinutes)
	// Resolve the target date in the viewer's timezone, then compare each row's
	// local date against it. Bounding by UTC time first would need the same
	// offset maths anyway, and request_logs is small enough that the scan is cheap.
	dayExpr := "date(time" + tzMod + ")"
	targetExpr := fmt.Sprintf("date('now'%s, '-%d days')", tzMod, dayOffset)

	var out TrayDayUsage
	err := d.db.QueryRow(`
		SELECT ` + targetExpr + `,
			COALESCE(COUNT(*), 0),
			COALESCE(SUM` + totalTokensExpr + `, 0),
			` + breakdownCols + `
		FROM request_logs
		WHERE ` + dayExpr + ` = ` + targetExpr).
		Scan(append([]any{&out.Date, &out.RequestCount, &out.TotalTokens}, out.scanArgs()...)...)
	if err != nil {
		return out, err
	}

	rows, err := d.db.Query(`
		SELECT api_key_name,
			COUNT(*),
			COALESCE(SUM` + totalTokensExpr + `, 0),
			COALESCE(SUM(cost_usd), 0)
		FROM request_logs
		WHERE ` + dayExpr + ` = ` + targetExpr + `
			AND api_key_name != ''
		GROUP BY api_key_name
		ORDER BY SUM` + totalTokensExpr + ` DESC`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var k TrayKeyUsage
		if err := rows.Scan(&k.KeyName, &k.RequestCount, &k.TotalTokens, &k.CostUSD); err != nil {
			return out, err
		}
		out.ByKey = append(out.ByKey, k)
	}
	return out, rows.Err()
}

// DailyAverage returns the mean requests and tokens per day over the days
// immediately preceding today, excluding today itself so a partial day can't
// drag the baseline down. Days with no traffic still count as days, otherwise a
// quiet week would inflate the average.
func (d *DB) DailyAverage(days, tzMinutes int) (avgRequests, avgTokens, avgCost float64, err error) {
	if days <= 0 {
		return 0, 0, 0, nil
	}
	tzMod := tzOffset(tzMinutes)
	dayExpr := "date(time" + tzMod + ")"
	todayExpr := fmt.Sprintf("date('now'%s)", tzMod)

	var reqs, toks int
	var cost float64
	err = d.db.QueryRow(`
		SELECT COALESCE(COUNT(*), 0),
			COALESCE(SUM`+totalTokensExpr+`, 0),
			COALESCE(SUM(cost_usd), 0)
		FROM request_logs
		WHERE `+dayExpr+` < `+todayExpr+`
			AND `+dayExpr+` >= `+fmt.Sprintf("date('now'%s, '-%d days')", tzMod, days)).
		Scan(&reqs, &toks, &cost)
	if err != nil {
		return 0, 0, 0, err
	}
	d64 := float64(days)
	return float64(reqs) / d64, float64(toks) / d64, cost / d64, nil
}

// HourlyToday returns per-hour buckets for the current calendar day in the
// viewer's timezone, for the tray sparkline. Empty hours are filled with zeros
// so the sparkline has a stable 24-slot shape.
func (d *DB) HourlyToday(tzMinutes int) ([]int, error) {
	tzMod := tzOffset(tzMinutes)
	rows, err := d.db.Query(`
		SELECT CAST(strftime('%H', time` + tzMod + `) AS INTEGER),
			COALESCE(SUM` + totalTokensExpr + `, 0)
		FROM request_logs
		WHERE date(time` + tzMod + `) = date('now'` + tzMod + `)
		GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buckets := make([]int, 24)
	for rows.Next() {
		var h, toks int
		if err := rows.Scan(&h, &toks); err != nil {
			return nil, err
		}
		if h >= 0 && h < 24 {
			buckets[h] = toks
		}
	}
	return buckets, rows.Err()
}

// LastRequestTime returns the timestamp of the most recent logged request, so
// the tray can show staleness ("no traffic for 3h") instead of a bare zero.
func (d *DB) LastRequestTime() (time.Time, bool) {
	var s string
	if err := d.db.QueryRow(`SELECT time FROM request_logs ORDER BY time DESC LIMIT 1`).Scan(&s); err != nil {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
