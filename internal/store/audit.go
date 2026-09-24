package store

import (
	"database/sql"
	"encoding/json"
)

type AuditEntry struct {
	ID            int64
	ActorUsername string
	Action        string
	ResourceType  string
	ResourceID    string
	Metadata      string
	CreatedAt     string
}

// Audit records a privileged or notable action. metadata may be nil.
func (s *Store) Audit(userID int64, actorUsername, action, resType, resID string, metadata map[string]any) error {
	var meta string
	if metadata != nil {
		if b, err := json.Marshal(metadata); err == nil {
			meta = string(b)
		}
	}
	var uid any
	if userID > 0 {
		uid = userID
	}
	_, err := s.db.Exec(`
		INSERT INTO audit_logs
			(user_id, actor_username, action, resource_type, resource_id, metadata, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		uid, actorUsername, action, resType, resID, meta, nowUTC())
	return err
}

// RecentAudit returns the most recent audit entries (up to limit).
func (s *Store) RecentAudit(limit int) ([]AuditEntry, error) {
	return s.SearchAudit("", limit)
}

// SearchAudit filters audit entries by a query matching actor/action/resource.
// An empty query returns the most recent entries.
func (s *Store) SearchAudit(query string, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows *sql.Rows
	var err error
	if query == "" {
		rows, err = s.db.Query(`
			SELECT id, actor_username, action, resource_type, resource_id, metadata, created_at
			FROM audit_logs ORDER BY id DESC LIMIT ?`, limit)
	} else {
		like := "%" + query + "%"
		rows, err = s.db.Query(`
			SELECT id, actor_username, action, resource_type, resource_id, metadata, created_at
			FROM audit_logs
			WHERE actor_username LIKE ? OR action LIKE ? OR resource_type LIKE ?
			   OR resource_id LIKE ? OR metadata LIKE ?
			ORDER BY id DESC LIMIT ?`, like, like, like, like, like, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.ActorUsername, &e.Action, &e.ResourceType,
			&e.ResourceID, &e.Metadata, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
