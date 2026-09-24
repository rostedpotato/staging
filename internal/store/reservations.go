package store

import (
	"database/sql"
	"errors"
	"time"
)

const (
	ResActive     = "active"
	ResReleased   = "released"
	ResExpired    = "expired"
	ResOverridden = "overridden"
)

// ErrConflict is returned when a booking overlaps an existing active one.
var ErrConflict = errors.New("slot already reserved for that time window")

type Reservation struct {
	ID        int64
	Slot      string
	UserID    int64
	Username  string
	Purpose   string
	Start     time.Time
	End       time.Time
	Status    string
	CreatedAt string
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// CreateReservation inserts an active booking after checking for overlap with
// other active bookings of the same slot. The check + insert run in one
// transaction so two simultaneous requests cannot both succeed.
func (s *Store) CreateReservation(slot string, userID int64, username, purpose string, start, end time.Time) (*Reservation, error) {
	if !end.After(start) {
		return nil, errors.New("end time must be after start time")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Overlap: existing.start < new.end AND existing.end > new.start.
	var n int
	err = tx.QueryRow(`
		SELECT COUNT(*) FROM reservations
		WHERE slot = ? AND status = 'active'
		  AND start_time < ? AND end_time > ?`,
		slot, end.UTC().Format(time.RFC3339), start.UTC().Format(time.RFC3339)).Scan(&n)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, ErrConflict
	}

	now := nowUTC()
	res, err := tx.Exec(`
		INSERT INTO reservations
			(slot, user_id, username, purpose, start_time, end_time, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'active', ?)`,
		slot, userID, username, purpose,
		start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Reservation{
		ID: id, Slot: slot, UserID: userID, Username: username, Purpose: purpose,
		Start: start, End: end, Status: ResActive, CreatedAt: now,
	}, nil
}

// ActiveForSlot returns the current active, non-expired reservation for a slot,
// or nil. It first lazily marks past-due active rows as expired.
func (s *Store) ActiveForSlot(slot string) (*Reservation, error) {
	s.ExpireDue()
	row := s.db.QueryRow(`
		SELECT id, slot, user_id, username, purpose, start_time, end_time, status, created_at
		FROM reservations
		WHERE slot = ? AND status = 'active' AND start_time <= ? AND end_time > ?
		ORDER BY start_time LIMIT 1`,
		slot, nowUTC(), nowUTC())
	return scanRes(row)
}

// ListActive returns all active reservations (current + upcoming).
func (s *Store) ListActive() ([]Reservation, error) {
	s.ExpireDue()
	rows, err := s.db.Query(`
		SELECT id, slot, user_id, username, purpose, start_time, end_time, status, created_at
		FROM reservations WHERE status = 'active' ORDER BY slot, start_time`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanResList(rows)
}

// ListForUser returns recent reservations made by a user.
func (s *Store) ListForUser(userID int64, limit int) ([]Reservation, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT id, slot, user_id, username, purpose, start_time, end_time, status, created_at
		FROM reservations WHERE user_id = ? ORDER BY id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanResList(rows)
}

func (s *Store) ReservationByID(id int64) (*Reservation, error) {
	row := s.db.QueryRow(`
		SELECT id, slot, user_id, username, purpose, start_time, end_time, status, created_at
		FROM reservations WHERE id = ?`, id)
	return scanRes(row)
}

// Release ends a reservation with the given final status (released/overridden).
func (s *Store) Release(id int64, status string) error {
	r, err := s.db.Exec(`
		UPDATE reservations SET status = ?, released_at = ?
		WHERE id = ? AND status = 'active'`, status, nowUTC(), id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return errors.New("reservation not active or not found")
	}
	return nil
}

// ExpireDue flags active reservations whose end_time has passed as expired.
func (s *Store) ExpireDue() {
	_, _ = s.db.Exec(`
		UPDATE reservations SET status = 'expired', released_at = ?
		WHERE status = 'active' AND end_time <= ?`, nowUTC(), nowUTC())
}

func scanRes(row rowScanner) (*Reservation, error) {
	var r Reservation
	var start, end string
	err := row.Scan(&r.ID, &r.Slot, &r.UserID, &r.Username, &r.Purpose,
		&start, &end, &r.Status, &r.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r.Start, r.End = parseTime(start), parseTime(end)
	return &r, nil
}

func scanResList(rows *sql.Rows) ([]Reservation, error) {
	var out []Reservation
	for rows.Next() {
		var r Reservation
		var start, end string
		if err := rows.Scan(&r.ID, &r.Slot, &r.UserID, &r.Username, &r.Purpose,
			&start, &end, &r.Status, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Start, r.End = parseTime(start), parseTime(end)
		out = append(out, r)
	}
	return out, rows.Err()
}
