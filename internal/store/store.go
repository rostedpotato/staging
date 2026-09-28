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

func (s *Store) Close() error { return s.db.Close() }

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }
