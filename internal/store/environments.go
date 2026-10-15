package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

const (
	ServerStaging    = "staging"
	ServerAutomation = "automation"

	EnvActive  = "active"
	EnvMissing = "missing"

	DesiredRunning = "running"
	DesiredStopped = "stopped"
)

// Environment is one known env (slot) and the ports its services publish.
type Environment struct {
	Name   string
	Server string
	Ports  map[string]int
	Status string
	// DesiredState is "stopped" when an admin switched the env off on purpose.
	DesiredState string
	// DesiredChangedAt is when DesiredState last changed (zero if never).
	DesiredChangedAt time.Time
	FirstSeen        string
	LastSeen         string
}

// EnvChange is one row of a discovery diff.
type EnvChange struct {
	Name     string         `json:"name"`
	Server   string         `json:"server"`
	Kind     string         `json:"kind"` // new | changed | missing | unchanged | back
	OldPorts map[string]int `json:"oldPorts,omitempty"`
	Ports    map[string]int `json:"ports,omitempty"`
}

// ListEnvironments returns envs ordered by server then name. If server is
// non-empty, only that server's envs are returned.
func (s *Store) ListEnvironments(server string) ([]Environment, error) {
	q := `SELECT name, server, ports, status, desired_state, desired_changed_at, first_seen, last_seen FROM environments`
	var args []any
	if server != "" {
		q += ` WHERE server = ?`
		args = append(args, server)
	}
	q += ` ORDER BY server DESC, name`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Environment
	for rows.Next() {
		var e Environment
		var ports, changed string
		if err := rows.Scan(&e.Name, &e.Server, &ports, &e.Status, &e.DesiredState, &changed, &e.FirstSeen, &e.LastSeen); err != nil {
			return nil, err
		}
		e.DesiredChangedAt = parseTime(changed)
		e.Ports = map[string]int{}
		_ = json.Unmarshal([]byte(ports), &e.Ports)
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetEnvironment returns one env by name, or nil if it is not in the inventory.
func (s *Store) GetEnvironment(name string) (*Environment, error) {
	envs, err := s.ListEnvironments("")
	if err != nil {
		return nil, err
	}
	for i := range envs {
		if envs[i].Name == name {
			return &envs[i], nil
		}
	}
	return nil, nil
}

// SetDesiredState records whether an env should be running or deliberately
// stopped. It reports sql.ErrNoRows-like absence as an error so a typo'd env
// name can't silently do nothing.
func (s *Store) SetDesiredState(name, state string) error {
	if state != DesiredRunning && state != DesiredStopped {
		return errors.New("invalid desired state")
	}
	now := nowUTC()
	res, err := s.db.Exec(`UPDATE environments SET desired_state = ?, desired_changed_at = ?, updated_at = ? WHERE name = ?`,
		state, now, now, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("unknown env " + name)
	}
	return nil
}

// EnvironmentServer returns which server an env lives on, or "" if unknown.
func (s *Store) EnvironmentServer(name string) (string, error) {
	var server string
	err := s.db.QueryRow(`SELECT server FROM environments WHERE name = ?`, name).Scan(&server)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return server, err
}

// CountEnvironments is used to decide whether to seed the inventory.
func (s *Store) CountEnvironments() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM environments`).Scan(&n)
	return n, err
}

// ApplyEnvChanges writes a reviewed discovery diff in one transaction:
// new/changed/back envs are upserted as active, missing ones are flagged.
func (s *Store) ApplyEnvChanges(changes []EnvChange) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowUTC()
	for _, c := range changes {
		switch c.Kind {
		case "missing":
			if _, err := tx.Exec(`UPDATE environments SET status = ?, updated_at = ? WHERE name = ?`,
				EnvMissing, now, c.Name); err != nil {
				return err
			}
		default:
			ports, _ := json.Marshal(c.Ports)
			if _, err := tx.Exec(`
				INSERT INTO environments (name, server, ports, status, first_seen, last_seen, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(name) DO UPDATE SET
					server = excluded.server, ports = excluded.ports, status = excluded.status,
					last_seen = excluded.last_seen, updated_at = excluded.updated_at`,
				c.Name, c.Server, string(ports), EnvActive, now, now, now); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// DiffEnvironments compares what discovery found on the given servers with
// the stored inventory. Envs on a server that was NOT scanned (or whose scan
// failed) are left out, so they are never wrongly marked missing.
func DiffEnvironments(stored []Environment, found []EnvChange, scanned map[string]bool) []EnvChange {
	byName := map[string]Environment{}
	for _, e := range stored {
		byName[e.Name] = e
	}
	seen := map[string]bool{}
	var out []EnvChange
	for _, f := range found {
		seen[f.Name] = true
		old, ok := byName[f.Name]
		// A stopped container publishes no ports in `docker ps`, so a service
		// that was found without a port keeps the one already stored.
		ports := map[string]int{}
		for svc, p := range old.Ports {
			ports[svc] = p
		}
		for svc, p := range f.Ports {
			ports[svc] = p
		}
		f.Ports = ports
		c := EnvChange{Name: f.Name, Server: f.Server, Ports: f.Ports, OldPorts: old.Ports}
		switch {
		case !ok:
			c.Kind = "new"
		case old.Status == EnvMissing:
			c.Kind = "back"
		case !samePorts(old.Ports, f.Ports) || old.Server != f.Server:
			c.Kind = "changed"
		default:
			c.Kind = "unchanged"
		}
		out = append(out, c)
	}
	for _, e := range stored {
		if seen[e.Name] || !scanned[e.Server] || e.Status == EnvMissing {
			continue
		}
		out = append(out, EnvChange{Name: e.Name, Server: e.Server, Kind: "missing", OldPorts: e.Ports})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server != out[j].Server {
			return out[i].Server > out[j].Server
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func samePorts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
