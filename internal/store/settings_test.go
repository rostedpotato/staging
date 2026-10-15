package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAutoPowerSetting(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if on, _ := st.AutoPower(); on {
		t.Fatal("auto power must default to off")
	}
	if err := st.SetAutoPower(true); err != nil {
		t.Fatal(err)
	}
	on, since := st.AutoPower()
	if !on || time.Since(since) > time.Minute {
		t.Fatalf("want on with a fresh timestamp, got %v %v", on, since)
	}
	if err := st.SetAutoPower(false); err != nil {
		t.Fatal(err)
	}
	if on, _ := st.AutoPower(); on {
		t.Fatal("want off")
	}
}

func TestLastBookingEnds_UsesEarlyRelease(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u, _ := st.CreateUser("a", "a", RoleEngineering, "pass1234")
	now := time.Now().UTC().Truncate(time.Second)
	r, err := st.CreateReservation("dev-04", u.ID, "a", "", now.Add(-time.Hour), now.Add(5*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Release(r.ID, ResReleased); err != nil {
		t.Fatal(err)
	}
	ends, err := st.LastBookingEnds()
	if err != nil {
		t.Fatal(err)
	}
	if got := ends["dev-04"]; got.After(now.Add(time.Minute)) || got.Before(now.Add(-time.Minute)) {
		t.Fatalf("early release should count as the end (~now), got %s", got)
	}
}
