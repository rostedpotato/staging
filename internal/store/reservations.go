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

// ConflictError wraps ErrConflict with who already holds the clashing
// booking, so the UI can tell the requester who to go negotiate with.
type ConflictError struct {
	Username string
	Until    time.Time
}

func (e *ConflictError) Error() string { return ErrConflict.Error() }
func (e *ConflictError) Unwrap() error { return ErrConflict }

type Reservation struct {
	ID         int64
	Slot       string
	UserID     int64
	Username   string
	Purpose    string
	Start      time.Time
	End        time.Time
	Status     string
	CreatedAt  string
	ReleasedAt time.Time // zero if still active/never released
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

	// Overlap: existing.start < new.end AND existing.end > new.start. Grab
	// who holds it too, so the caller can show "booked by X until Y" instead
	// of a generic conflict message.
	var otherUser string
	var otherEnd string
	err = tx.QueryRow(`
		SELECT username, end_time FROM reservations
		WHERE slot = ? AND status = 'active'
		  AND start_time < ? AND end_time > ?
		LIMIT 1`,
		slot, end.UTC().Format(time.RFC3339), start.UTC().Format(time.RFC3339)).
		Scan(&otherUser, &otherEnd)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		return nil, &ConflictError{Username: otherUser, Until: parseTime(otherEnd)}
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
		SELECT id, slot, user_id, username, purpose, start_time, end_time, status, created_at, released_at
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
		SELECT id, slot, user_id, username, purpose, start_time, end_time, status, created_at, released_at
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
		SELECT id, slot, user_id, username, purpose, start_time, end_time, status, created_at, released_at
		FROM reservations WHERE user_id = ? ORDER BY id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanResList(rows)
}

// ListHistory returns every reservation (any status) whose window overlaps
// [from, to], newest start first - used by the dashboard to show a rolling
// window of past/upcoming bookings across all slots, with the exact
// start/end time (not just whole-day), since a booking can be released early.
func (s *Store) ListHistory(from, to time.Time) ([]Reservation, error) {
	s.ExpireDue()
	rows, err := s.db.Query(`
		SELECT id, slot, user_id, username, purpose, start_time, end_time, status, created_at, released_at
		FROM reservations
		WHERE start_time < ? AND end_time > ?
		ORDER BY start_time DESC`,
		to.UTC().Format(time.RFC3339), from.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanResList(rows)
}

func (s *Store) ReservationByID(id int64) (*Reservation, error) {
	row := s.db.QueryRow(`
		SELECT id, slot, user_id, username, purpose, start_time, end_time, status, created_at, released_at
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
	var released sql.NullString
	err := row.Scan(&r.ID, &r.Slot, &r.UserID, &r.Username, &r.Purpose,
		&start, &end, &r.Status, &r.CreatedAt, &released)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r.Start, r.End = parseTime(start), parseTime(end)
	if released.Valid {
		r.ReleasedAt = parseTime(released.String)
	}
	return &r, nil
}

func scanResList(rows *sql.Rows) ([]Reservation, error) {
	var out []Reservation
	for rows.Next() {
		var r Reservation
		var start, end string
		var released sql.NullString
		if err := rows.Scan(&r.ID, &r.Slot, &r.UserID, &r.Username, &r.Purpose,
			&start, &end, &r.Status, &r.CreatedAt, &released); err != nil {
			return nil, err
		}
		r.Start, r.End = parseTime(start), parseTime(end)
		if released.Valid {
			r.ReleasedAt = parseTime(released.String)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
