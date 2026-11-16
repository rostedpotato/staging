package store

import (
	"errors"
	"testing"
	"time"
)

// bookSlot creates an active whole-range booking for a provisioned user.
func bookSlot(t *testing.T, st *Store, slot, username string, start, end time.Time) (*User, *Reservation) {
	t.Helper()
	u, err := st.CreateUser(username, username, RoleEngineering, "TempPass123!")
	if err != nil {
		t.Fatal(err)
	}
	res, err := st.CreateReservationWithDetails(slot, u.ID, u.Username, u.Username, "team-x", "testing", start, end)
	if err != nil {
		t.Fatal(err)
	}
	return u, res
}

func TestRequestCollaborator_HappyPath(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	_, res := bookSlot(t, st, "dev-01", "qa1", now, now.Add(24*time.Hour))
	eng, err := st.CreateUser("eng1", "Eng One", RoleEngineering, "TempPass123!")
	if err != nil {
		t.Fatal(err)
	}

	c, err := st.RequestCollaborator(res.ID, eng.ID, eng.Username, eng.Name, "team-y")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != CollabPending {
		t.Fatalf("status = %q, want pending", c.Status)
	}

	// Owner is already a holder; the pending co-owner is not yet.
	if ok, _ := st.IsHolder(*res, eng.ID); ok {
		t.Fatal("pending co-owner must not be a holder yet")
	}

	if err := st.DecideCollaborator(c.ID, true, res.UserID); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.IsHolder(*res, eng.ID); err != nil || !ok {
		t.Fatalf("approved co-owner must be a holder: ok=%v err=%v", ok, err)
	}
	// Owner remains a holder too.
	if ok, _ := st.IsHolder(*res, res.UserID); !ok {
		t.Fatal("owner must remain a holder")
	}
}

func TestRequestCollaborator_OwnerCannotRequestOwnBooking(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	owner, res := bookSlot(t, st, "dev-01", "qa1", now, now.Add(24*time.Hour))

	_, err := st.RequestCollaborator(res.ID, owner.ID, owner.Username, owner.Name, "team-x")
	if !errors.Is(err, ErrCollabIsOwner) {
		t.Fatalf("got %v, want ErrCollabIsOwner", err)
	}
}

func TestRequestCollaborator_OnlyOneActiveAtATime(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	_, res := bookSlot(t, st, "dev-01", "qa1", now, now.Add(24*time.Hour))
	eng1, _ := st.CreateUser("eng1", "Eng One", RoleEngineering, "TempPass123!")
	eng2, _ := st.CreateUser("eng2", "Eng Two", RoleEngineering, "TempPass123!")

	if _, err := st.RequestCollaborator(res.ID, eng1.ID, eng1.Username, eng1.Name, "t"); err != nil {
		t.Fatal(err)
	}
	// Second request while the first is still pending must be rejected.
	if _, err := st.RequestCollaborator(res.ID, eng2.ID, eng2.Username, eng2.Name, "t"); !errors.Is(err, ErrCollabExists) {
		t.Fatalf("got %v, want ErrCollabExists", err)
	}
}

func TestRequestCollaborator_NewRequestAllowedAfterPreviousGone(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	_, res := bookSlot(t, st, "dev-01", "qa1", now, now.Add(24*time.Hour))
	eng1, _ := st.CreateUser("eng1", "Eng One", RoleEngineering, "TempPass123!")
	eng2, _ := st.CreateUser("eng2", "Eng Two", RoleEngineering, "TempPass123!")

	c1, err := st.RequestCollaborator(res.ID, eng1.ID, eng1.Username, eng1.Name, "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DecideCollaborator(c1.ID, false, res.UserID); err != nil { // rejected
		t.Fatal(err)
	}
	// Rejected is no longer "active"; a new request must be allowed.
	if _, err := st.RequestCollaborator(res.ID, eng2.ID, eng2.Username, eng2.Name, "t"); err != nil {
		t.Fatalf("new request after rejection: %v", err)
	}

	// Approve eng2, then have them leave (revoke); a third request must open up.
	c2, err := st.MyCollaboratorRequest(res.ID, eng2.ID)
	if err != nil || c2 == nil {
		t.Fatalf("expected pending request for eng2: %v", err)
	}
	if err := st.DecideCollaborator(c2.ID, true, res.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RequestCollaborator(res.ID, eng1.ID, eng1.Username, eng1.Name, "t"); !errors.Is(err, ErrCollabExists) {
		t.Fatalf("got %v, want ErrCollabExists while eng2 approved", err)
	}
	if err := st.RevokeCollaborator(c2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RequestCollaborator(res.ID, eng1.ID, eng1.Username, eng1.Name, "t"); err != nil {
		t.Fatalf("new request after revoke: %v", err)
	}
}

func TestRequestCollaborator_InactiveReservationRejected(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	_, res := bookSlot(t, st, "dev-01", "qa1", now, now.Add(24*time.Hour))
	eng, _ := st.CreateUser("eng1", "Eng One", RoleEngineering, "TempPass123!")

	if err := st.Release(res.ID, ResReleased); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RequestCollaborator(res.ID, eng.ID, eng.Username, eng.Name, "t"); err == nil {
		t.Fatal("expected error requesting co-ownership of a released booking")
	}
}

func TestDecideCollaborator_OnlyPendingCanBeDecided(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	_, res := bookSlot(t, st, "dev-01", "qa1", now, now.Add(24*time.Hour))
	eng, _ := st.CreateUser("eng1", "Eng One", RoleEngineering, "TempPass123!")
	c, err := st.RequestCollaborator(res.ID, eng.ID, eng.Username, eng.Name, "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DecideCollaborator(c.ID, true, res.UserID); err != nil {
		t.Fatal(err)
	}
	// Deciding again must fail: no longer pending.
	if err := st.DecideCollaborator(c.ID, true, res.UserID); !errors.Is(err, ErrCollabNotFound) {
		t.Fatalf("got %v, want ErrCollabNotFound", err)
	}
	if err := st.DecideCollaborator(99999, true, res.UserID); !errors.Is(err, ErrCollabNotFound) {
		t.Fatalf("unknown id: got %v, want ErrCollabNotFound", err)
	}
}

func TestRevokeCollaborator_OnlyApprovedCanBeRevoked(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	_, res := bookSlot(t, st, "dev-01", "qa1", now, now.Add(24*time.Hour))
	eng, _ := st.CreateUser("eng1", "Eng One", RoleEngineering, "TempPass123!")
	c, err := st.RequestCollaborator(res.ID, eng.ID, eng.Username, eng.Name, "t")
	if err != nil {
		t.Fatal(err)
	}
	// Still pending: revoke must fail (use DecideCollaborator to reject instead).
	if err := st.RevokeCollaborator(c.ID); !errors.Is(err, ErrCollabNotFound) {
		t.Fatalf("got %v, want ErrCollabNotFound", err)
	}
	if err := st.DecideCollaborator(c.ID, true, res.UserID); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeCollaborator(c.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.IsHolder(*res, eng.ID); ok {
		t.Fatal("revoked co-owner must no longer be a holder")
	}
	// Revoking again must fail: no longer approved.
	if err := st.RevokeCollaborator(c.ID); !errors.Is(err, ErrCollabNotFound) {
		t.Fatalf("second revoke: got %v, want ErrCollabNotFound", err)
	}
}

func TestIsHolder_InactiveReservationHasNoHolders(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	owner, res := bookSlot(t, st, "dev-01", "qa1", now, now.Add(24*time.Hour))
	eng, _ := st.CreateUser("eng1", "Eng One", RoleEngineering, "TempPass123!")
	c, err := st.RequestCollaborator(res.ID, eng.ID, eng.Username, eng.Name, "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DecideCollaborator(c.ID, true, res.UserID); err != nil {
		t.Fatal(err)
	}
	if err := st.Release(res.ID, ResReleased); err != nil {
		t.Fatal(err)
	}
	released, err := st.ReservationByID(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.IsHolder(*released, owner.ID); ok {
		t.Fatal("released booking: owner must no longer be a holder")
	}
	if ok, _ := st.IsHolder(*released, eng.ID); ok {
		t.Fatal("released booking: co-owner must no longer be a holder")
	}
}

func TestIsHolder_StrangerIsNotAHolder(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	_, res := bookSlot(t, st, "dev-01", "qa1", now, now.Add(24*time.Hour))
	stranger, _ := st.CreateUser("stranger", "Stranger", RoleEngineering, "TempPass123!")
	if ok, err := st.IsHolder(*res, stranger.ID); err != nil || ok {
		t.Fatalf("ok=%v err=%v, want false/nil", ok, err)
	}
}

func TestPendingRequestsForOwner(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	owner, res := bookSlot(t, st, "dev-01", "qa1", now, now.Add(24*time.Hour))
	eng, _ := st.CreateUser("eng1", "Eng One", RoleEngineering, "TempPass123!")

	if list, err := st.PendingRequestsForOwner(owner.ID); err != nil || len(list) != 0 {
		t.Fatalf("list=%v err=%v, want empty", list, err)
	}
	if _, err := st.RequestCollaborator(res.ID, eng.ID, eng.Username, eng.Name, "t"); err != nil {
		t.Fatal(err)
	}
	list, err := st.PendingRequestsForOwner(owner.ID)
	if err != nil || len(list) != 1 || list[0].UserID != eng.ID {
		t.Fatalf("list=%+v err=%v", list, err)
	}
}

func TestActiveCollaboratorsForReservations(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	_, res1 := bookSlot(t, st, "dev-01", "qa1", now, now.Add(24*time.Hour))
	_, res2 := bookSlot(t, st, "dev-02", "qa2", now, now.Add(24*time.Hour))
	eng, _ := st.CreateUser("eng1", "Eng One", RoleEngineering, "TempPass123!")

	c, err := st.RequestCollaborator(res1.ID, eng.ID, eng.Username, eng.Name, "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DecideCollaborator(c.ID, true, res1.UserID); err != nil {
		t.Fatal(err)
	}

	m, err := st.ActiveCollaboratorsForReservations([]int64{res1.ID, res2.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 {
		t.Fatalf("map=%+v, want exactly res1", m)
	}
	if got, ok := m[res1.ID]; !ok || got.UserID != eng.ID {
		t.Fatalf("res1 entry = %+v, ok=%v", got, ok)
	}
	if _, ok := m[res2.ID]; ok {
		t.Fatal("res2 must have no approved collaborator")
	}

	// Empty input must not error and must return an empty map.
	if m2, err := st.ActiveCollaboratorsForReservations(nil); err != nil || len(m2) != 0 {
		t.Fatalf("m2=%v err=%v", m2, err)
	}
}

func TestMyCollaboratorRequest(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	_, res := bookSlot(t, st, "dev-01", "qa1", now, now.Add(24*time.Hour))
	eng, _ := st.CreateUser("eng1", "Eng One", RoleEngineering, "TempPass123!")

	if c, err := st.MyCollaboratorRequest(res.ID, eng.ID); err != nil || c != nil {
		t.Fatalf("c=%v err=%v, want nil", c, err)
	}
	if _, err := st.RequestCollaborator(res.ID, eng.ID, eng.Username, eng.Name, "t"); err != nil {
		t.Fatal(err)
	}
	c, err := st.MyCollaboratorRequest(res.ID, eng.ID)
	if err != nil || c == nil || c.Status != CollabPending {
		t.Fatalf("c=%+v err=%v", c, err)
	}
}

func TestCollaboratorsForReservation_KeepsHistory(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	_, res := bookSlot(t, st, "dev-01", "qa1", now, now.Add(24*time.Hour))
	eng1, _ := st.CreateUser("eng1", "Eng One", RoleEngineering, "TempPass123!")
	eng2, _ := st.CreateUser("eng2", "Eng Two", RoleEngineering, "TempPass123!")

	c1, _ := st.RequestCollaborator(res.ID, eng1.ID, eng1.Username, eng1.Name, "t")
	_ = st.DecideCollaborator(c1.ID, false, res.UserID)
	c2, _ := st.RequestCollaborator(res.ID, eng2.ID, eng2.Username, eng2.Name, "t")
	_ = st.DecideCollaborator(c2.ID, true, res.UserID)

	list, err := st.CollaboratorsForReservation(res.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("list=%+v err=%v, want 2 historical rows", list, err)
	}
}
