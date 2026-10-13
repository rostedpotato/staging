package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

const (
	RunRunning   = "running"
	RunReady     = "ready"
	RunApplied   = "applied"
	RunDiscarded = "discarded"
	RunFailed    = "failed"
)

// ErrRunInProgress is returned when a discovery is already running or waiting
// for review: only one draft may exist at a time.
var ErrRunInProgress = errors.New("a discovery run is already running or waiting for review")

// DiscoveryResult is the proposed change set stored with a run.
type DiscoveryResult struct {
	Changes []EnvChange       `json:"changes"`
	Errors  map[string]string `json:"errors,omitempty"` // server -> error
}

type DiscoveryRun struct {
	ID         int64
	StartedBy  string
	Servers    []string
	Status     string
	Result     DiscoveryResult
	Error      string
	StartedAt  string
	FinishedAt sql.NullString
	DecidedBy  string
	DecidedAt  sql.NullString
}

// StartDiscoveryRun records a new run, refusing if one is still open.
func (s *Store) StartDiscoveryRun(startedBy string, servers []string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var open int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM discovery_runs WHERE status IN (?, ?)`,
		RunRunning, RunReady).Scan(&open); err != nil {
		return 0, err
	}
	if open > 0 {
		return 0, ErrRunInProgress
	}
	res, err := tx.Exec(`INSERT INTO discovery_runs (started_by, servers, status, started_at) VALUES (?, ?, ?, ?)`,
		startedBy, strings.Join(servers, ","), RunRunning, nowUTC())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, tx.Commit()
}

// FinishDiscoveryRun stores the result. A run where every server failed is
// marked failed (nothing to review); otherwise it's ready for review.
func (s *Store) FinishDiscoveryRun(id int64, result DiscoveryResult, allFailed bool) error {
	b, _ := json.Marshal(result)
	status, errMsg := RunReady, ""
	if allFailed {
		status = RunFailed
		var parts []string
		for server, e := range result.Errors {
			parts = append(parts, server+": "+e)
		}
		errMsg = strings.Join(parts, "; ")
	}
	_, err := s.db.Exec(`UPDATE discovery_runs SET status = ?, result_json = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, string(b), errMsg, nowUTC(), id)
	return err
}

// DecideDiscoveryRun moves a ready run to applied or discarded. It only
// succeeds once: a second decision on the same run returns false.
func (s *Store) DecideDiscoveryRun(id int64, status, decidedBy string) (bool, error) {
	res, err := s.db.Exec(`UPDATE discovery_runs SET status = ?, decided_by = ?, decided_at = ? WHERE id = ? AND status = ?`,
		status, decidedBy, nowUTC(), id, RunReady)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// FailStaleDiscoveryRuns closes runs left "running" by a restart.
func (s *Store) FailStaleDiscoveryRuns() error {
	_, err := s.db.Exec(`UPDATE discovery_runs SET status = ?, error = 'interrupted by restart', finished_at = ? WHERE status = ?`,
		RunFailed, nowUTC(), RunRunning)
	return err
}

func (s *Store) DiscoveryRunByID(id int64) (*DiscoveryRun, error) {
	runs, err := s.queryRuns(`WHERE id = ?`, id)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	return &runs[0], nil
}

// RecentDiscoveryRuns returns the latest runs, newest first.
func (s *Store) RecentDiscoveryRuns(limit int) ([]DiscoveryRun, error) {
	return s.queryRuns(`ORDER BY id DESC LIMIT ?`, limit)
}

func (s *Store) queryRuns(where string, args ...any) ([]DiscoveryRun, error) {
	rows, err := s.db.Query(`SELECT id, started_by, servers, status, result_json, error, started_at,
		finished_at, decided_by, decided_at FROM discovery_runs `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DiscoveryRun
	for rows.Next() {
		var r DiscoveryRun
		var servers, result string
		if err := rows.Scan(&r.ID, &r.StartedBy, &servers, &r.Status, &result, &r.Error,
			&r.StartedAt, &r.FinishedAt, &r.DecidedBy, &r.DecidedAt); err != nil {
			return nil, err
		}
		r.Servers = strings.Split(servers, ",")
		_ = json.Unmarshal([]byte(result), &r.Result)
		out = append(out, r)
	}
	return out, rows.Err()
}
