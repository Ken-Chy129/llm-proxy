package stats

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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

type DB struct {
	db *sql.DB
}

func Open(dir string) (*DB, error) {
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".cli-proxy")
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
			api_key_name      TEXT DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_logs_time ON request_logs(time);
		CREATE INDEX IF NOT EXISTS idx_logs_model ON request_logs(model);
	`)
	if err != nil {
		return err
	}
	// Add api_key_name column if missing (existing DBs)
	db.Exec("ALTER TABLE request_logs ADD COLUMN api_key_name TEXT DEFAULT ''")
	return nil
}

func (d *DB) Record(log *RequestLog) error {
	_, err := d.db.Exec(`
		INSERT INTO request_logs (time, model, backend, latency_ms, status, prompt_tokens, completion_tokens, stream, error, api_key_name)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		log.Time.UTC().Format(time.RFC3339), log.Model, log.Backend, log.LatencyMs,
		log.Status, log.PromptTokens, log.CompletionTokens, log.Stream, log.Error, log.APIKeyName,
	)
	return err
}

func (d *DB) QueryLogs(limit, offset int) ([]RequestLog, int, error) {
	if limit <= 0 {
		limit = 50
	}

	var total int
	d.db.QueryRow("SELECT COUNT(*) FROM request_logs").Scan(&total)

	rows, err := d.db.Query(`
		SELECT id, time, model, backend, latency_ms, status, prompt_tokens, completion_tokens, stream, error, api_key_name
		FROM request_logs ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var logs []RequestLog
	for rows.Next() {
		var l RequestLog
		var t string
		if err := rows.Scan(&l.ID, &t, &l.Model, &l.Backend, &l.LatencyMs, &l.Status,
			&l.PromptTokens, &l.CompletionTokens, &l.Stream, &l.Error, &l.APIKeyName); err != nil {
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
