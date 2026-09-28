package store

import (
	"testing"
	"time"
)

// TestFixLegacyUTCBookings verifies the one-time repair for reservations
// created before the container had TZ=Asia/Jakarta set (when time.Local
// resolved to UTC), which stored whole-day WIB bookings as 00:00:00Z..
// 23:59:59Z instead of the correct 17:00:00Z(prev day)..16:59:59Z window.
func TestFixLegacyUTCBookings(t *testing.T) {
	dbPath := t.TempDir() + "/legacy.db"
	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	u, err := st.CreateUser("bob", "Bob", RoleEngineering, "TempPass123!")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a legacy row: whole-day booking stored in raw UTC clock time
	// (the bug), plus a correctly-formed WIB row that must NOT be touched.
	if _, err := st.db.Exec(`
		INSERT INTO reservations (slot, user_id, username, purpose, start_time, end_time, status, created_at)
		VALUES ('legacy-slot', ?, 'bob', 'legacy', '2026-09-28T00:00:00Z', '2026-09-28T23:59:59Z', 'active', ?)`,
		u.ID, nowUTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`
		INSERT INTO reservations (slot, user_id, username, purpose, start_time, end_time, status, created_at)
		VALUES ('correct-slot', ?, 'bob', 'correct', '2026-09-27T17:00:00Z', '2026-09-28T16:59:59Z', 'active', ?)`,
		u.ID, nowUTC()); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Reopen: fixLegacyUTCBookings runs again on every Open (idempotent).
	st2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	all, err := st2.ListHistory(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), "", "date_desc")
	if err != nil {
		t.Fatal(err)
	}
	byPurpose := map[string]Reservation{}
	for _, r := range all {
		byPurpose[r.Purpose] = r
	}

	legacy, ok := byPurpose["legacy"]
	if !ok {
		t.Fatal("legacy booking missing")
	}
	wantStart := time.Date(2026, 9, 27, 17, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 9, 28, 16, 59, 59, 0, time.UTC)
	if !legacy.Start.Equal(wantStart) {
		t.Fatalf("legacy start: want %v, got %v", wantStart, legacy.Start)
	}
	if !legacy.End.Equal(wantEnd) {
		t.Fatalf("legacy end: want %v, got %v", wantEnd, legacy.End)
	}

	correct, ok := byPurpose["correct"]
	if !ok {
		t.Fatal("correct booking missing")
	}
	if !correct.Start.Equal(wantStart) || !correct.End.Equal(wantEnd) {
		t.Fatal("already-correct booking should be untouched, but its times changed")
	}

	// Reopening a third time must not shift the now-corrected legacy row again.
	st2.Close()
	st3, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st3.Close()
	all3, err := st3.ListHistory(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), "", "date_desc")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all3 {
		if r.Purpose == "legacy" && !r.Start.Equal(wantStart) {
			t.Fatalf("idempotency broken: legacy start shifted again to %v", r.Start)
		}
	}
}
