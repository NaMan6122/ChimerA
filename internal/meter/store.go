// Package meter records per-request usage in SQLite and serves quota checks.
//
// Single source of truth is the requests table; monthly counts are plain
// SELECTs (tens of thousands of rows scan instantly at this scale).
// Fail-open: all errors propagate so callers can log and continue.
package meter

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Record is one metered provider attempt.
type Record struct {
	TS              time.Time
	Tenant          string
	Provider        string
	Model           string
	Streaming       bool
	PromptChars     int
	CompletionChars int
	LatencyMs       int64
	Code            int
}

// Usage is an aggregate over a time range.
type Usage struct {
	Requests        int            `json:"requests"`
	PromptChars     int64          `json:"prompt_chars"`
	CompletionChars int64          `json:"completion_chars"`
	Errors          int            `json:"errors"`
	ByProvider      map[string]int `json:"by_provider"`
}

// Store wraps the usage database.
type Store struct {
	db *sql.DB
}

// Open creates parent dirs, opens (or creates) the database, and migrates schema.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("meter: empty database path")
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("meter: creating dir: %w", err)
		}
	}
	// Busy timeout keeps concurrent stream/non-stream writers from failing.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout%3D5000&_pragma=journal_mode%3DWAL")
	if err != nil {
		return nil, fmt.Errorf("meter: open: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS requests(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts INTEGER NOT NULL,
		tenant TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		streaming INTEGER NOT NULL DEFAULT 0,
		prompt_chars INTEGER NOT NULL DEFAULT 0,
		completion_chars INTEGER NOT NULL DEFAULT 0,
		latency_ms INTEGER NOT NULL DEFAULT 0,
		code INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		return fmt.Errorf("meter: migrate: %w", err)
	}
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_requests_tenant_ts ON requests(tenant, ts)`)
	if err != nil {
		return fmt.Errorf("meter: index: %w", err)
	}
	return nil
}

// Record appends one usage row.
func (s *Store) Record(r Record) error {
	streaming := 0
	if r.Streaming {
		streaming = 1
	}
	_, err := s.db.Exec(`INSERT INTO requests
		(ts, tenant, provider, model, streaming, prompt_chars, completion_chars, latency_ms, code)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.TS.Unix(), r.Tenant, r.Provider, r.Model, streaming,
		r.PromptChars, r.CompletionChars, r.LatencyMs, r.Code)
	if err != nil {
		return fmt.Errorf("meter: record: %w", err)
	}
	return nil
}

// MonthBounds returns [start, end) UTC for the month containing t.
func MonthBounds(t time.Time) (time.Time, time.Time) {
	t = t.UTC()
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// MonthCount counts a tenant's requests in the month containing t.
func (s *Store) MonthCount(tenant string, t time.Time) (int, error) {
	start, end := MonthBounds(t)
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM requests
		WHERE tenant = ? AND ts >= ? AND ts < ?`,
		tenant, start.Unix(), end.Unix()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("meter: month count: %w", err)
	}
	return n, nil
}

// Summarize aggregates a tenant's usage over the inclusive window [from, to].
func (s *Store) Summarize(tenant string, from, to time.Time) (Usage, error) {
	u := Usage{ByProvider: map[string]int{}}
	row := s.db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(prompt_chars),0), COALESCE(SUM(completion_chars),0),
		COALESCE(SUM(CASE WHEN code >= 500 THEN 1 ELSE 0 END),0)
		FROM requests WHERE tenant = ? AND ts >= ? AND ts <= ?`,
		tenant, from.Unix(), to.Unix())
	if err := row.Scan(&u.Requests, &u.PromptChars, &u.CompletionChars, &u.Errors); err != nil {
		return u, fmt.Errorf("meter: summarize: %w", err)
	}
	rows, err := s.db.Query(`SELECT provider, COUNT(*) FROM requests
		WHERE tenant = ? AND ts >= ? AND ts <= ? GROUP BY provider`,
		tenant, from.Unix(), to.Unix())
	if err != nil {
		return u, fmt.Errorf("meter: by-provider: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var provider string
		var n int
		if err := rows.Scan(&provider, &n); err != nil {
			return u, fmt.Errorf("meter: scan: %w", err)
		}
		u.ByProvider[provider] = n
	}
	return u, rows.Err()
}
