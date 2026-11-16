package store

import (
	"database/sql"
	"errors"
	"time"
)

// Collaborator statuses. Only one pending/approved row may exist per
// reservation at a time (enforced transactionally in RequestCollaborator);
// rejected/revoked rows are kept as history and never reused.
const (
	CollabPending  = "pending"
	CollabApproved = "approved"
	CollabRejected = "rejected"
	CollabRevoked  = "revoked"
)

// ErrCollabExists is returned when a reservation already has an active
// (pending or approved) co-owner request.
var ErrCollabExists = errors.New("this booking already has an active co-owner request")

// ErrCollabIsOwner is returned when the owner tries to request co-ownership
// of their own booking.
var ErrCollabIsOwner = errors.New("you already own this booking")

// ErrCollabNotFound is returned when a collaborator row/decision target does
// not exist.
var ErrCollabNotFound = errors.New("co-owner request not found")

type Collaborator struct {
	ID            int64
	ReservationID int64
	UserID        int64
	Username      string
	Name          string
	Team          string
	Status        string
	RequestedAt   string
	DecidedAt     time.Time // zero if not yet decided
	DecidedBy     sql.NullInt64
}

// RequestCollaborator records a request to co-own reservationID. Fails with
// ErrCollabIsOwner if the requester is the booking's owner, or
// ErrCollabExists if the booking already has a pending/approved co-owner.
// The existence check and the insert run in one transaction so two
// simultaneous requests cannot both succeed.
func (s *Store) RequestCollaborator(reservationID, userID int64, username, name, team string) (*Collaborator, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var ownerID int64
	var resStatus string
	err = tx.QueryRow(`SELECT user_id, status FROM reservations WHERE id = ?`, reservationID).
		Scan(&ownerID, &resStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("reservation not found")
	}
	if err != nil {
		return nil, err
	}
	if resStatus != ResActive {
		return nil, errors.New("this booking is no longer active")
	}
	if ownerID == userID {
		return nil, ErrCollabIsOwner
	}

	var exists int
	err = tx.QueryRow(`
		SELECT 1 FROM reservation_collaborators
		WHERE reservation_id = ? AND status IN ('pending','approved') LIMIT 1`,
		reservationID).Scan(&exists)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		return nil, ErrCollabExists
	}

	now := nowUTC()
	res, err := tx.Exec(`
		INSERT INTO reservation_collaborators
			(reservation_id, user_id, username, name, team, status, requested_at)
		VALUES (?, ?, ?, ?, ?, 'pending', ?)`,
		reservationID, userID, username, name, team, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Collaborator{
		ID: id, ReservationID: reservationID, UserID: userID, Username: username,
		Name: name, Team: team, Status: CollabPending, RequestedAt: now,
	}, nil
}

// DecideCollaborator approves or rejects a pending request. Callers must
// verify the decider is the reservation's owner before calling this.
func (s *Store) DecideCollaborator(id int64, approve bool, decidedBy int64) error {
	status := CollabRejected
	if approve {
		status = CollabApproved
	}
	r, err := s.db.Exec(`
		UPDATE reservation_collaborators SET status = ?, decided_at = ?, decided_by = ?
		WHERE id = ? AND status = 'pending'`,
		status, nowUTC(), decidedBy, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrCollabNotFound
	}
	return nil
}

// RevokeCollaborator ends an approved co-ownership: the co-owner leaving on
// their own, or the owner/an admin removing them. Only an 'approved' row can
// be revoked (a 'pending' request is rejected instead, via DecideCollaborator).
//
// Both cases land on the same 'revoked' status - there is no separate 'left'
// status. Who ended it (self vs. owner/admin) is recorded only in the web
// layer's audit log action (LEAVE_COLLAB vs REVOKE_COLLAB; see bookings.go),
// not as a distinct store-level status. This is intentional: every place
// that treats a collaborator as "currently active" only ever checks
// status == approved (IsHolder, CollaboratorsForReservations, ...). Adding a
// 'left' status would mean auditing and updating every one of those checks
// for no behavioral difference - revoked and left both mean "not a holder
// anymore". If a future requirement needs to distinguish them in the store
// (not just in the audit trail), prefer adding it as a new column
// (e.g. revoked_by, or a left_voluntarily bool) rather than a new status
// value, so existing status == approved/revoked checks keep working.
func (s *Store) RevokeCollaborator(id int64) error {
	r, err := s.db.Exec(`
		UPDATE reservation_collaborators SET status = 'revoked', decided_at = ?
		WHERE id = ? AND status = 'approved'`,
		nowUTC(), id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrCollabNotFound
	}
	return nil
}

// CollaboratorByID fetches one collaborator row, or nil if not found.
func (s *Store) CollaboratorByID(id int64) (*Collaborator, error) {
	return scanCollab(s.db.QueryRow(`
		SELECT id, reservation_id, user_id, username, name, team, status, requested_at, decided_at, decided_by
		FROM reservation_collaborators WHERE id = ?`, id))
}

// CollaboratorsForReservation lists every co-owner request (any status) for
// a reservation, most recent first.
func (s *Store) CollaboratorsForReservation(reservationID int64) ([]Collaborator, error) {
	rows, err := s.db.Query(`
		SELECT id, reservation_id, user_id, username, name, team, status, requested_at, decided_at, decided_by
		FROM reservation_collaborators WHERE reservation_id = ? ORDER BY id DESC`, reservationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCollabList(rows)
}

// CollaboratorsForReservations batches the "currently relevant" collaborator
// rows (pending or approved - never the rejected/revoked history) for many
// reservations in a single query, keyed by reservation id. This is what the
// bookings list needs: unlike CollaboratorsForReservation (singular), it
// must be called once for the whole page, not once per row, or every page
// load does one query per active booking (N+1). If a future caller needs
// full history for many reservations too, add a separate batch helper
// instead of widening this one's WHERE clause.
func (s *Store) CollaboratorsForReservations(reservationIDs []int64) (map[int64][]Collaborator, error) {
	out := map[int64][]Collaborator{}
	if len(reservationIDs) == 0 {
		return out, nil
	}
	q := `SELECT id, reservation_id, user_id, username, name, team, status, requested_at, decided_at, decided_by
	      FROM reservation_collaborators
	      WHERE status IN ('pending','approved') AND reservation_id IN (?` +
		repeatPlaceholders(len(reservationIDs)-1) + `)`
	args := make([]any, len(reservationIDs))
	for i, id := range reservationIDs {
		args[i] = id
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanCollabList(rows)
	if err != nil {
		return nil, err
	}
	for _, c := range list {
		out[c.ReservationID] = append(out[c.ReservationID], c)
	}
	return out, nil
}

// PendingRequestsForOwner lists pending co-owner requests the given user (as
// reservation owner) needs to decide on.
func (s *Store) PendingRequestsForOwner(ownerUserID int64) ([]Collaborator, error) {
	rows, err := s.db.Query(`
		SELECT rc.id, rc.reservation_id, rc.user_id, rc.username, rc.name, rc.team,
		       rc.status, rc.requested_at, rc.decided_at, rc.decided_by
		FROM reservation_collaborators rc
		JOIN reservations r ON r.id = rc.reservation_id
		WHERE rc.status = 'pending' AND r.user_id = ? AND r.status = 'active'
		ORDER BY rc.id`, ownerUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCollabList(rows)
}

// MyCollaboratorRequest returns the given user's own pending/approved request
// on a reservation, or nil.
func (s *Store) MyCollaboratorRequest(reservationID, userID int64) (*Collaborator, error) {
	return scanCollab(s.db.QueryRow(`
		SELECT id, reservation_id, user_id, username, name, team, status, requested_at, decided_at, decided_by
		FROM reservation_collaborators
		WHERE reservation_id = ? AND user_id = ? AND status IN ('pending','approved')
		LIMIT 1`, reservationID, userID))
}

// IsHolder reports whether userID may act as a holder of res: its owner, or
// an approved co-owner. A reservation that is no longer active (released,
// expired, overridden) has no holders at all, even its original owner -
// callers checking "may deploy / may release" must always pass a freshly
// loaded Reservation so this reflects the current status.
func (s *Store) IsHolder(res Reservation, userID int64) (bool, error) {
	if res.Status != ResActive {
		return false, nil
	}
	if res.UserID == userID {
		return true, nil
	}
	var exists int
	err := s.db.QueryRow(`
		SELECT 1 FROM reservation_collaborators
		WHERE reservation_id = ? AND user_id = ? AND status = 'approved' LIMIT 1`,
		res.ID, userID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func repeatPlaceholders(n int) string {
	s := ""
	for i := 0; i < n; i++ {
		s += ",?"
	}
	return s
}

func scanCollab(row rowScanner) (*Collaborator, error) {
	var c Collaborator
	var decidedAt sql.NullString
	err := row.Scan(&c.ID, &c.ReservationID, &c.UserID, &c.Username, &c.Name, &c.Team,
		&c.Status, &c.RequestedAt, &decidedAt, &c.DecidedBy)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if decidedAt.Valid {
		c.DecidedAt = parseTime(decidedAt.String)
	}
	return &c, nil
}

func scanCollabList(rows *sql.Rows) ([]Collaborator, error) {
	var out []Collaborator
	for rows.Next() {
		var c Collaborator
		var decidedAt sql.NullString
		if err := rows.Scan(&c.ID, &c.ReservationID, &c.UserID, &c.Username, &c.Name, &c.Team,
			&c.Status, &c.RequestedAt, &decidedAt, &c.DecidedBy); err != nil {
			return nil, err
		}
		if decidedAt.Valid {
			c.DecidedAt = parseTime(decidedAt.String)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
