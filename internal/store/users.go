package store

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"math/big"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID                int64
	Username          string
	Name              string
	PasswordHash      string
	MustResetPassword bool
	Role              string // engineering | admin
	Status            string // active | disabled
	CreatedAt         string
	LastLogin         sql.NullString
}

const (
	RoleEngineering = "engineering"
	RoleAdmin       = "admin"
)

var (
	// ErrUserExists is returned by CreateUser when the username is already registered.
	ErrUserExists = errors.New("a user with this username already exists")
	// ErrInvalidCredentials is returned by VerifyPassword for a wrong username/password.
	ErrInvalidCredentials = errors.New("invalid username or password")
)

const passwordChars = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789!@#$%"

// GenerateTempPassword returns a random, human-typeable password (12 chars)
// for a newly provisioned or reset account.
func GenerateTempPassword() (string, error) {
	b := make([]byte, 12)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(passwordChars))))
		if err != nil {
			return "", err
		}
		b[i] = passwordChars[n.Int64()]
	}
	return string(b), nil
}

func hashPassword(plain string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// CreateUser provisions a NEW account with a temporary password that the user
// must change on first login. Accounts are always created this way (by an
// admin, or once at bootstrap) — logging in never creates an account.
func (s *Store) CreateUser(username, name, role, tempPassword string) (*User, error) {
	hash, err := hashPassword(tempPassword)
	if err != nil {
		return nil, err
	}
	now := nowUTC()
	_, err = s.db.Exec(`
		INSERT INTO users (username, name, password_hash, must_reset_password, role, status, created_at)
		VALUES (?, ?, ?, 1, ?, 'active', ?)`,
		username, name, hash, role, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrUserExists
		}
		return nil, err
	}
	return s.UserByUsername(username)
}

// VerifyPassword checks credentials and, on success, touches last_login.
// Returns ErrInvalidCredentials for a wrong username/password or a disabled
// account, without revealing which.
func (s *Store) VerifyPassword(username, password string) (*User, error) {
	u, err := s.UserByUsername(username)
	if err != nil {
		return nil, err
	}
	if u == nil || !u.IsActive() {
		return nil, ErrInvalidCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return nil, ErrInvalidCredentials
	}
	_, _ = s.db.Exec(`UPDATE users SET last_login = ? WHERE id = ?`, nowUTC(), u.ID)
	return s.UserByUsername(username)
}

// SetPassword updates a user's own password and clears must_reset_password.
func (s *Store) SetPassword(id int64, newPassword string) error {
	hash, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		UPDATE users SET password_hash = ?, must_reset_password = 0 WHERE id = ?`,
		hash, id)
	return err
}

// ResetPassword (admin action) assigns a new random temp password and forces
// the user to change it on next login. Returns the plaintext to hand out.
func (s *Store) ResetPassword(id int64) (string, error) {
	temp, err := GenerateTempPassword()
	if err != nil {
		return "", err
	}
	hash, err := hashPassword(temp)
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(`
		UPDATE users SET password_hash = ?, must_reset_password = 1 WHERE id = ?`,
		hash, id)
	if err != nil {
		return "", err
	}
	return temp, nil
}

// CountUsers returns the number of registered users (for admin bootstrap).
func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CountAdmins returns the number of active admin users.
func (s *Store) CountAdmins() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin' AND status = 'active'`).Scan(&n)
	return n, err
}

// ListUsers returns all users ordered by username.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`
		SELECT id, username, name, password_hash, must_reset_password, role, status, created_at, last_login
		FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// SetRole changes a user's role (engineering|admin).
func (s *Store) SetRole(id int64, role string) error {
	_, err := s.db.Exec(`UPDATE users SET role = ? WHERE id = ?`, role, id)
	return err
}

// SetStatus enables/disables a user (active|disabled).
func (s *Store) SetStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE users SET status = ? WHERE id = ?`, status, id)
	return err
}

func (s *Store) UserByUsername(username string) (*User, error) {
	row := s.db.QueryRow(`
		SELECT id, username, name, password_hash, must_reset_password, role, status, created_at, last_login
		FROM users WHERE username = ?`, username)
	return scanUser(row)
}

func (s *Store) UserByID(id int64) (*User, error) {
	row := s.db.QueryRow(`
		SELECT id, username, name, password_hash, must_reset_password, role, status, created_at, last_login
		FROM users WHERE id = ?`, id)
	return scanUser(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(row rowScanner) (*User, error) {
	var u User
	var mustReset int
	if err := row.Scan(&u.ID, &u.Username, &u.Name, &u.PasswordHash, &mustReset,
		&u.Role, &u.Status, &u.CreatedAt, &u.LastLogin); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	u.MustResetPassword = mustReset != 0
	return &u, nil
}

func (u *User) IsAdmin() bool  { return u != nil && u.Role == RoleAdmin }
func (u *User) IsActive() bool { return u != nil && u.Status == "active" }
