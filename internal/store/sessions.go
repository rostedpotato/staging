package store

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

type Session struct {
	ID        string
	UserID    int64
	ExpiresAt time.Time
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// CreateSession issues a new session token for a user, valid for ttl.
func (s *Store) CreateSession(userID int64, ttl time.Duration) (*Session, error) {
	tok, err := newToken()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	exp := now.Add(ttl)
	_, err = s.db.Exec(`
		INSERT INTO sessions (id, user_id, created_at, expires_at)
		VALUES (?, ?, ?, ?)`,
		tok, userID, now.Format(time.RFC3339), exp.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	return &Session{ID: tok, UserID: userID, ExpiresAt: exp}, nil
}

// UserForSession returns the active user for a valid, unexpired session token.
// Expired or unknown tokens yield (nil, nil).
func (s *Store) UserForSession(token string) (*User, error) {
	if token == "" {
		return nil, nil
	}
	row := s.db.QueryRow(`
		SELECT u.id, u.username, u.name, u.password_hash, u.must_reset_password,
		       u.role, u.status, u.created_at, u.last_login
		FROM sessions se JOIN users u ON u.id = se.user_id
		WHERE se.id = ? AND se.expires_at > ?`,
		token, time.Now().UTC().Format(time.RFC3339))
	u, err := scanUser(row)
	if err != nil || u == nil || !u.IsActive() {
		return nil, err
	}
	return u, nil
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, token)
	return err
}

// PurgeExpiredSessions removes stale sessions; call periodically.
func (s *Store) PurgeExpiredSessions() {
	_, _ = s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`,
		time.Now().UTC().Format(time.RFC3339))
}
