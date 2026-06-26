package stats

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type RequestLog struct {
	ID               int64     `json:"id"`
	Time             time.Time `json:"time"`
	Model            string    `json:"model"`
	Backend          string    `json:"backend"`
	LatencyMs        int64     `json:"latency_ms"`
	Status           int       `json:"status"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	Stream           bool      `json:"stream"`
	Error            string    `json:"error,omitempty"`
	APIKeyName       string    `json:"api_key_name,omitempty"`
	Account          string    `json:"account,omitempty"`       // upstream account (email/id) that served the request
	FailoverFrom     string    `json:"failover_from,omitempty"` // comma-separated accounts that 429'd before the serving one
}

type KeyStats struct {
	KeyName         string `json:"key_name"`
	RequestCount    int    `json:"request_count"`
	TotalTokens     int    `json:"total_tokens"`
	ErrorCount      int    `json:"error_count"`
	TokensToday     int    `json:"tokens_today"`
}

type ModelStats struct {
	Model            string  `json:"model"`
	RequestCount     int     `json:"request_count"`
	TotalPrompt      int     `json:"total_prompt_tokens"`
	TotalCompletion  int     `json:"total_completion_tokens"`
	AvgLatencyMs     float64 `json:"avg_latency_ms"`
	ErrorCount       int     `json:"error_count"`
}

type DailyStats struct {
	Date             string `json:"date"`
	RequestCount     int    `json:"request_count"`
	TotalPrompt      int    `json:"total_prompt_tokens"`
	TotalCompletion  int    `json:"total_completion_tokens"`
	ErrorCount       int    `json:"error_count"`
}

// BucketStats is one point on the time-series trend (hourly or daily bucket).
type BucketStats struct {
	Bucket           string `json:"bucket"`
	RequestCount     int    `json:"request_count"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	ErrorCount       int    `json:"error_count"`
}

// DimStats is one row of a generic dimension breakdown (per model/key/backend/account).
type DimStats struct {
	Label        string  `json:"label"`
	RequestCount int     `json:"request_count"`
	TotalTokens  int     `json:"total_tokens"`
	ErrorCount   int     `json:"error_count"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
}

// dimensionColumns whitelists the groupable columns so the dimension name can be
// safely interpolated into the GROUP BY (it never comes from untrusted SQL).
var dimensionColumns = map[string]string{
	"model":   "model",
	"backend": "backend",
	"key":     "api_key_name",
	"account": "account",
	"status":  "status", // error categorization; only failed rows (status >= 400)
}

// DimensionColumn resolves a dimension name (model/key/backend/account) to its
// underlying column, or "" when unknown. Used by callers to build filters.
func DimensionColumn(dim string) string { return dimensionColumns[dim] }

// filterClause returns an extra "AND col = ?" plus its arg when col is a known
// column and val is non-empty. col is validated against the whitelisted columns
// so it can be interpolated into SQL safely; "(none)" maps back to the empty
// string actually stored in the row.
func filterClause(col, val string) (string, []any) {
	if col == "" || val == "" {
		return "", nil
	}
	known := false
	for _, c := range dimensionColumns {
		if c == col {
			known = true
			break
		}
	}
	if !known {
		return "", nil
	}
	if val == "(none)" {
		val = ""
	}
	return " AND " + col + " = ?", []any{val}
}

type DB struct {
	db *sql.DB
}

func Open(dir string) (*DB, error) {
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".llm-proxy")
	}
	os.MkdirAll(dir, 0700)

	dbPath := filepath.Join(dir, "stats.db")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open stats db: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	return &DB{db: db}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS request_logs (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			time              DATETIME DEFAULT (datetime('now')),
			model             TEXT,
			backend           TEXT,
			latency_ms        INTEGER,
			status            INTEGER,
			prompt_tokens     INTEGER DEFAULT 0,
			completion_tokens INTEGER DEFAULT 0,
			stream            BOOLEAN DEFAULT 0,
			error             TEXT DEFAULT '',
			api_key_name      TEXT DEFAULT '',
			account           TEXT DEFAULT '',
			failover_from     TEXT DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_logs_time ON request_logs(time);
		CREATE INDEX IF NOT EXISTS idx_logs_model ON request_logs(model);
	`)
	if err != nil {
		return err
	}
	// Add api_key_name column if missing (existing DBs)
	db.Exec("ALTER TABLE request_logs ADD COLUMN api_key_name TEXT DEFAULT ''")
	// Add account column if missing (existing DBs)
	db.Exec("ALTER TABLE request_logs ADD COLUMN account TEXT DEFAULT ''")
	// Add failover_from column if missing (existing DBs)
	db.Exec("ALTER TABLE request_logs ADD COLUMN failover_from TEXT DEFAULT ''")
	return nil
}

func (d *DB) Record(log *RequestLog) error {
	_, err := d.db.Exec(`
		INSERT INTO request_logs (time, model, backend, latency_ms, status, prompt_tokens, completion_tokens, stream, error, api_key_name, account, failover_from)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		log.Time.UTC().Format(time.RFC3339), log.Model, log.Backend, log.LatencyMs,
		log.Status, log.PromptTokens, log.CompletionTokens, log.Stream, log.Error, log.APIKeyName, log.Account, log.FailoverFrom,
	)
	return err
}

func (d *DB) QueryLogs(limit, offset int, errorsOnly bool, search string) ([]RequestLog, int, error) {
	if limit <= 0 {
		limit = 50
	}

	// Build the shared WHERE clause for both the count and the page query.
	where := "WHERE 1=1"
	var args []any
	if errorsOnly {
		where += " AND status >= 400"
	}
	if search = strings.TrimSpace(search); search != "" {
		where += " AND (model LIKE ? OR account LIKE ? OR api_key_name LIKE ? OR error LIKE ?)"
		like := "%" + search + "%"
		args = append(args, like, like, like, like)
	}

	var total int
	d.db.QueryRow("SELECT COUNT(*) FROM request_logs "+where, args...).Scan(&total)

	rows, err := d.db.Query(`
		SELECT id, time, model, backend, latency_ms, status, prompt_tokens, completion_tokens, stream, error, api_key_name, account, failover_from
		FROM request_logs `+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var logs []RequestLog
	for rows.Next() {
		var l RequestLog
		var t string
		if err := rows.Scan(&l.ID, &t, &l.Model, &l.Backend, &l.LatencyMs, &l.Status,
			&l.PromptTokens, &l.CompletionTokens, &l.Stream, &l.Error, &l.APIKeyName, &l.Account, &l.FailoverFrom); err != nil {
			continue
		}
		l.Time, _ = time.Parse(time.RFC3339, t)
		logs = append(logs, l)
	}
	return logs, total, nil
}

func (d *DB) StatsByModel(daysBack int) ([]ModelStats, error) {
	since := time.Now().AddDate(0, 0, -daysBack).UTC().Format(time.RFC3339)
	rows, err := d.db.Query(`
		SELECT model,
			COUNT(*) as cnt,
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(AVG(latency_ms), 0),
			COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END), 0)
		FROM request_logs
		WHERE time >= ?
		GROUP BY model
		ORDER BY cnt DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ModelStats
	for rows.Next() {
		var s ModelStats
		rows.Scan(&s.Model, &s.RequestCount, &s.TotalPrompt, &s.TotalCompletion, &s.AvgLatencyMs, &s.ErrorCount)
		result = append(result, s)
	}
	return result, nil
}

func (d *DB) StatsByDay(daysBack int) ([]DailyStats, error) {
	since := time.Now().AddDate(0, 0, -daysBack).UTC().Format(time.RFC3339)
	rows, err := d.db.Query(`
		SELECT date(time) as d,
			COUNT(*),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END), 0)
		FROM request_logs
		WHERE time >= ?
		GROUP BY d
		ORDER BY d DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []DailyStats
	for rows.Next() {
		var s DailyStats
		rows.Scan(&s.Date, &s.RequestCount, &s.TotalPrompt, &s.TotalCompletion, &s.ErrorCount)
		result = append(result, s)
	}
	return result, nil
}

// StatsByBucket returns the request/token/error counts grouped into time buckets
// over the last daysBack days. granularity is "hour" (bucket key like
// 2026-06-25T14:00) or anything else for "day" (2026-06-25). tzMinutes shifts
// the bucket boundaries to the viewer's timezone (minutes east of UTC) so the
// chart reads in local time regardless of where the server runs. Buckets with
// no traffic are omitted; callers fill the continuous axis.
func (d *DB) StatsByBucket(daysBack, tzMinutes int, granularity, filterCol, filterVal string) ([]BucketStats, error) {
	since := time.Now().AddDate(0, 0, -daysBack).UTC().Format(time.RFC3339)
	tzMod := fmt.Sprintf(", '%+d minutes'", tzMinutes)
	bucketExpr := "date(time" + tzMod + ")"
	if granularity == "hour" {
		bucketExpr = "strftime('%Y-%m-%dT%H:00', time" + tzMod + ")"
	}
	fc, fargs := filterClause(filterCol, filterVal)
	rows, err := d.db.Query(`
		SELECT `+bucketExpr+` as b,
			COUNT(*),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END), 0)
		FROM request_logs
		WHERE time >= ?`+fc+`
		GROUP BY b
		ORDER BY b ASC`, append([]any{since}, fargs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []BucketStats
	for rows.Next() {
		var s BucketStats
		rows.Scan(&s.Bucket, &s.RequestCount, &s.PromptTokens, &s.CompletionTokens, &s.ErrorCount)
		result = append(result, s)
	}
	return result, nil
}

// StatsByDimension groups traffic over the last daysBack days by one of the
// whitelisted dimensions (model/backend/key/account). For key and account the
// empty label is filtered out. Returns an error for an unknown dimension.
func (d *DB) StatsByDimension(dimension string, daysBack int, filterCol, filterVal string) ([]DimStats, error) {
	col, ok := dimensionColumns[dimension]
	if !ok {
		return nil, fmt.Errorf("unknown dimension %q", dimension)
	}
	since := time.Now().AddDate(0, 0, -daysBack).UTC().Format(time.RFC3339)
	colExpr := col
	where := "time >= ?"
	if dimension == "key" || dimension == "account" {
		where += " AND " + col + " != ''"
	}
	if dimension == "status" {
		colExpr = "CAST(status AS TEXT)" // numeric code → text label
		where += " AND status >= 400"    // only failed requests
	}
	fc, fargs := filterClause(filterCol, filterVal)
	rows, err := d.db.Query(`
		SELECT `+colExpr+` as label,
			COUNT(*),
			COALESCE(SUM(prompt_tokens + completion_tokens), 0),
			COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END), 0),
			COALESCE(AVG(latency_ms), 0)
		FROM request_logs
		WHERE `+where+fc+`
		GROUP BY label
		ORDER BY COUNT(*) DESC`, append([]any{since}, fargs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []DimStats
	for rows.Next() {
		var s DimStats
		rows.Scan(&s.Label, &s.RequestCount, &s.TotalTokens, &s.ErrorCount, &s.AvgLatencyMs)
		if s.Label == "" {
			s.Label = "(none)"
		}
		result = append(result, s)
	}
	return result, nil
}

// StatsSummary returns range-scoped totals, optionally filtered to one
// dimension value. Powers the summary strip above the charts.
func (d *DB) StatsSummary(daysBack int, filterCol, filterVal string) (requests, tokens, errors int, avgLatencyMs float64) {
	since := time.Now().AddDate(0, 0, -daysBack).UTC().Format(time.RFC3339)
	fc, fargs := filterClause(filterCol, filterVal)
	d.db.QueryRow(`
		SELECT COALESCE(COUNT(*),0),
			COALESCE(SUM(prompt_tokens + completion_tokens),0),
			COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END),0),
			COALESCE(AVG(latency_ms),0)
		FROM request_logs
		WHERE time >= ?`+fc, append([]any{since}, fargs...)...).
		Scan(&requests, &tokens, &errors, &avgLatencyMs)
	return
}

func (d *DB) StatsByKey() ([]KeyStats, error) {
	rows, err := d.db.Query(`
		SELECT api_key_name,
			COUNT(*),
			COALESCE(SUM(prompt_tokens + completion_tokens), 0),
			COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END), 0)
		FROM request_logs
		WHERE api_key_name != ''
		GROUP BY api_key_name
		ORDER BY COUNT(*) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	today := time.Now().UTC().Format("2006-01-02")
	var result []KeyStats
	for rows.Next() {
		var s KeyStats
		rows.Scan(&s.KeyName, &s.RequestCount, &s.TotalTokens, &s.ErrorCount)
		d.db.QueryRow(`
			SELECT COALESCE(SUM(prompt_tokens + completion_tokens), 0)
			FROM request_logs WHERE api_key_name = ? AND date(time) = ?`,
			s.KeyName, today).Scan(&s.TokensToday)
		result = append(result, s)
	}
	return result, nil
}

func (d *DB) TokensTodayForKey(keyName string) int {
	today := time.Now().UTC().Format("2006-01-02")
	var tokens int
	d.db.QueryRow(`
		SELECT COALESCE(SUM(prompt_tokens + completion_tokens), 0)
		FROM request_logs WHERE api_key_name = ? AND date(time) = ?`,
		keyName, today).Scan(&tokens)
	return tokens
}

func (d *DB) TotalStats() (requests int, tokens int, err error) {
	err = d.db.QueryRow(`
		SELECT COALESCE(COUNT(*), 0), COALESCE(SUM(prompt_tokens + completion_tokens), 0)
		FROM request_logs`).Scan(&requests, &tokens)
	return
}

// Cleanup deletes logs older than the given number of days.
func (d *DB) Cleanup(retainDays int) (int64, error) {
	if retainDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -retainDays).UTC().Format(time.RFC3339)
	result, err := d.db.Exec("DELETE FROM request_logs WHERE time < ?", cutoff)
	if err != nil {
		return 0, err
	}
	deleted, _ := result.RowsAffected()
	if deleted > 0 {
		d.db.Exec("VACUUM")
	}
	return deleted, nil
}

func (d *DB) Close() error {
	return d.db.Close()
}
