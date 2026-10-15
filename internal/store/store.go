// Package store is the SQLite persistence layer (pure-Go driver, no CGO).
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite DB at path and applies the schema.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	// Busy timeout + WAL keep the single-file DB happy under light concurrency.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite writer is single; avoids "database is locked".
	if err := db.Ping(); err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	st := &Store{db: db}
	if err := st.ensureColumn("environments", "desired_state", "TEXT NOT NULL DEFAULT 'running'"); err != nil {
		return nil, fmt.Errorf("add environments.desired_state: %w", err)
	}
	if err := st.ensureColumn("environments", "desired_changed_at", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, fmt.Errorf("add environments.desired_changed_at: %w", err)
	}
	if err := st.fixLegacyUTCBookings(); err != nil {
		return nil, fmt.Errorf("fix legacy UTC bookings: %w", err)
	}
	return st, nil
}

// fixLegacyUTCBookings repairs reservations created before the container had
// TZ=Asia/Jakarta set, when time.Local resolved to UTC. Whole-day bookings
// from that era were stored as 00:00:00Z..23:59:59Z instead of the intended
// 17:00:00Z (prev day)..16:59:59Z window for WIB midnight-to-midnight, which
// then displayed 7h into the next day (e.g. "until 06:59") once local time
// started rendering correctly. That exact pattern (:00 second-of-day start,
// 23:59:59 end) can never occur for a WIB whole-day booking, so matching on
// it is safe, precise, and naturally idempotent - once corrected, rows no
// longer match and won't be touched again.
func (s *Store) fixLegacyUTCBookings() error {
	_, err := s.db.Exec(`
		UPDATE reservations
		SET start_time = strftime('%Y-%m-%dT%H:%M:%SZ', start_time, '-7 hours'),
		    end_time   = strftime('%Y-%m-%dT%H:%M:%SZ', end_time, '-7 hours')
		WHERE substr(start_time, 12, 8) = '00:00:00'
		  AND substr(end_time, 12, 8) = '23:59:59'`)
	return err
}

// ensureColumn adds a column to a table created by an older schema. CREATE
// TABLE IF NOT EXISTS never alters an existing table, so databases made before
// a column was introduced need this one-time ALTER. table/column/def are
// compile-time constants from Open, never user input.
func (s *Store) ensureColumn(table, column, def string) error {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('` + table + `')`)
	if err != nil {
		return err
	}
	has := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			has = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close() // single connection: must be released before the ALTER
	if has {
		return nil
	}
	_, err = s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + def)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }
