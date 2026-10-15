package store

import (
	"path/filepath"
	"testing"
)

func TestDesiredState_DefaultsAndSet(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.ApplyEnvChanges([]EnvChange{{Name: "dev-04", Server: ServerStaging, Kind: "new"}}); err != nil {
		t.Fatal(err)
	}
	e, _ := st.GetEnvironment("dev-04")
	if e == nil || e.DesiredState != DesiredRunning {
		t.Fatalf("new env should default to running, got %+v", e)
	}
	if err := st.SetDesiredState("dev-04", DesiredStopped); err != nil {
		t.Fatal(err)
	}
	// A discovery re-save must not wake a stopped env up in the books.
	if err := st.ApplyEnvChanges([]EnvChange{{Name: "dev-04", Server: ServerStaging, Kind: "changed", Ports: map[string]int{"watersheep": 1}}}); err != nil {
		t.Fatal(err)
	}
	e, _ = st.GetEnvironment("dev-04")
	if e.DesiredState != DesiredStopped {
		t.Fatalf("discovery apply reset desired_state to %q", e.DesiredState)
	}
	if err := st.SetDesiredState("nope", DesiredStopped); err == nil {
		t.Fatal("unknown env must error")
	}
	if err := st.SetDesiredState("dev-04", "paused"); err == nil {
		t.Fatal("invalid state must error")
	}
}

func TestOpen_AddsDesiredStateToOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Recreate the table the way the previous release had it.
	for _, q := range []string{
		`DROP TABLE environments`,
		`CREATE TABLE environments (name TEXT PRIMARY KEY, server TEXT NOT NULL, ports TEXT NOT NULL DEFAULT '{}',
			status TEXT NOT NULL DEFAULT 'active', first_seen TEXT NOT NULL, last_seen TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`INSERT INTO environments VALUES ('ho','staging','{}','active','t','t','t')`,
	} {
		if _, err := st.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	st, err = Open(path) // must migrate, not fail
	if err != nil {
		t.Fatalf("reopen old db: %v", err)
	}
	defer st.Close()
	e, _ := st.GetEnvironment("ho")
	if e == nil || e.DesiredState != DesiredRunning {
		t.Fatalf("old row should become running, got %+v", e)
	}
	if st2, err := Open(path); err != nil { // second open: column already there
		t.Fatal(err)
	} else {
		st2.Close()
	}
}
