package store

import (
	"database/sql"
	"errors"
	"time"
)

const (
	DepQueued  = "queued"
	DepRunning = "running"
	DepSuccess = "success"
	DepFailed  = "failed"
	DepError   = "error"
)

// ErrSlotBusy is returned when a slot already has an in-flight deployment.
var ErrSlotBusy = errors.New("a deployment is already in progress for this slot")

type Deployment struct {
	ID           int64
	Slot         string
	Service      string
	RefType      string
	Ref          string
	UserID       int64
	Username     string
	Status       string
	JenkinsJob   string
	QueueURL     string
	BuildNumber  sql.NullInt64
	BuildURL     string
	ErrorMessage string
	CreatedAt    string
	StartedAt    sql.NullString
	FinishedAt   sql.NullString
}

func (d *Deployment) Active() bool { return d.Status == DepQueued || d.Status == DepRunning }

// CreateDeployment inserts a queued deployment and acquires the slot lock in one
// transaction. Returns ErrSlotBusy if the slot already has an active deploy.
func (s *Store) CreateDeployment(slot, service, refType, ref string, userID int64, username, job string) (*Deployment, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Acquire lock; PRIMARY KEY(slot) makes a second insert fail.
	now := nowUTC()
	res, err := tx.Exec(`
		INSERT INTO deployments
			(slot, service, ref_type, ref, user_id, username, status, jenkins_job, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'queued', ?, ?)`,
		slot, service, refType, ref, userID, username, job, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()

	if _, err := tx.Exec(`
		INSERT INTO slot_locks (slot, deployment_id, locked_at) VALUES (?, ?, ?)`,
		slot, id, now); err != nil {
		// UNIQUE violation on slot -> slot busy.
		return nil, ErrSlotBusy
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.DeploymentByID(id)
}

func (s *Store) DeploymentByID(id int64) (*Deployment, error) {
	row := s.db.QueryRow(depSelect+` WHERE id = ?`, id)
	return scanDep(row)
}

// ListDeployments returns recent deployments, optionally filtered by slot.
func (s *Store) ListDeployments(slot string, limit int) ([]Deployment, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if slot == "" {
		rows, err = s.db.Query(depSelect+` ORDER BY id DESC LIMIT ?`, limit)
	} else {
		rows, err = s.db.Query(depSelect+` WHERE slot = ? ORDER BY id DESC LIMIT ?`, slot, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		d, err := scanDepRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// PreviousSuccess returns the most recent successful deployment for a
// slot+service, excluding the given deployment id. Used for rollback targets.
func (s *Store) PreviousSuccess(slot, service string, excludeID int64) (*Deployment, error) {
	row := s.db.QueryRow(depSelect+`
		WHERE slot = ? AND service = ? AND status = 'success' AND id <> ?
		ORDER BY id DESC LIMIT 1`, slot, service, excludeID)
	return scanDep(row)
}

// SetQueue records the Jenkins queue URL after triggering the build.
func (s *Store) SetQueue(id int64, queueURL string) error {
	_, err := s.db.Exec(`UPDATE deployments SET queue_url = ? WHERE id = ?`, queueURL, id)
	return err
}

// MarkRunning sets status=running with the resolved build number/URL.
func (s *Store) MarkRunning(id int64, buildNumber int64, buildURL string) error {
	_, err := s.db.Exec(`
		UPDATE deployments SET status = 'running', build_number = ?, build_url = ?,
			started_at = COALESCE(started_at, ?)
		WHERE id = ? AND status IN ('queued','running')`,
		buildNumber, buildURL, nowUTC(), id)
	return err
}

// Finish sets a terminal status and releases the slot lock.
func (s *Store) Finish(id int64, status, errMsg string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		UPDATE deployments SET status = ?, error_message = ?, finished_at = ?
		WHERE id = ?`, status, errMsg, nowUTC(), id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM slot_locks WHERE deployment_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ActiveDeployments returns deployments still queued/running (for reconciliation).
func (s *Store) ActiveDeployments() ([]Deployment, error) {
	rows, err := s.db.Query(depSelect + ` WHERE status IN ('queued','running') ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		d, err := scanDepRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// --- logs -------------------------------------------------------------------

// AppendLog stores console lines starting at seq; returns the next seq.
func (s *Store) AppendLog(depID int64, startSeq int, lines []string) (int, error) {
	if len(lines) == 0 {
		return startSeq, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return startSeq, err
	}
	defer tx.Rollback()
	now := nowUTC()
	seq := startSeq
	for _, ln := range lines {
		if _, err := tx.Exec(`
			INSERT INTO deployment_logs (deployment_id, seq, line, created_at)
			VALUES (?, ?, ?, ?)`, depID, seq, ln, now); err != nil {
			return startSeq, err
		}
		seq++
	}
	return seq, tx.Commit()
}

// LogsSince returns log lines with seq >= sinceSeq.
func (s *Store) LogsSince(depID int64, sinceSeq int) ([]struct {
	Seq  int
	Line string
}, error) {
	rows, err := s.db.Query(`
		SELECT seq, line FROM deployment_logs
		WHERE deployment_id = ? AND seq >= ? ORDER BY seq`, depID, sinceSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		Seq  int
		Line string
	}
	for rows.Next() {
		var e struct {
			Seq  int
			Line string
		}
		if err := rows.Scan(&e.Seq, &e.Line); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LogCount returns how many log lines a deployment has (next seq).
func (s *Store) LogCount(depID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM deployment_logs WHERE deployment_id = ?`, depID).Scan(&n)
	return n, err
}

// --- scanning ---------------------------------------------------------------

const depSelect = `
	SELECT id, slot, service, ref_type, ref, user_id, username, status,
	       jenkins_job, queue_url, build_number, build_url, error_message,
	       created_at, started_at, finished_at
	FROM deployments`

func scanDep(row rowScanner) (*Deployment, error) {
	var d Deployment
	err := row.Scan(&d.ID, &d.Slot, &d.Service, &d.RefType, &d.Ref, &d.UserID,
		&d.Username, &d.Status, &d.JenkinsJob, &d.QueueURL, &d.BuildNumber,
		&d.BuildURL, &d.ErrorMessage, &d.CreatedAt, &d.StartedAt, &d.FinishedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &d, nil
}

func scanDepRows(rows *sql.Rows) (*Deployment, error) {
	var d Deployment
	err := rows.Scan(&d.ID, &d.Slot, &d.Service, &d.RefType, &d.Ref, &d.UserID,
		&d.Username, &d.Status, &d.JenkinsJob, &d.QueueURL, &d.BuildNumber,
		&d.BuildURL, &d.ErrorMessage, &d.CreatedAt, &d.StartedAt, &d.FinishedAt)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// CreatedTime parses the created_at for display/sort.
func (d *Deployment) CreatedTime() time.Time { return parseTime(d.CreatedAt) }
