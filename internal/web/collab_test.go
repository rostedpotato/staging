package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"parkee/staging-platform/internal/store"
)

// bookAs creates a one-day booking for u on slot via the real HTTP handler,
// and returns the created reservation's id (scraped from the Mine list).
func bookAs(t *testing.T, c *http.Client, ts string, st *store.Store, slot, name, team string) int64 {
	t.Helper()
	r := post(t, c, ts+"/bookings/create", url.Values{
		"slot": {slot}, "name": {name}, "team": {team}, "startDate": {day(0)}, "purpose": {"testing"},
	})
	r.Body.Close()
	if loc := r.Header.Get("Location"); !strings.Contains(loc, "notice=") {
		t.Fatalf("booking create failed: redirected to %s", loc)
	}
	active, err := st.ListActive()
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range active {
		if res.Slot == slot && res.Name == name {
			return res.ID
		}
	}
	t.Fatalf("booking for %s on %s not found after create", name, slot)
	return 0
}

// pendingCollabFor finds the single pending/approved collaborator row for a
// reservation directly in the store (handlers don't return an id in the
// redirect, so tests look it up like the UI would via CollaboratorsForReservation).
func collabFor(t *testing.T, st *store.Store, resID int64, username string) *store.Collaborator {
	t.Helper()
	list, err := st.CollaboratorsForReservation(resID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range list {
		if list[i].Username == username {
			return &list[i]
		}
	}
	return nil
}

func TestCollab_RequestAndApproveFlow(t *testing.T) {
	ts, st := newTestServer(t, "dev-01")
	qa := provisionAndLogin(t, ts, st, "qa1", store.RoleEngineering)
	eng := provisionAndLogin(t, ts, st, "eng1", store.RoleEngineering)

	resID := bookAs(t, qa, ts.URL, st, "dev-01", "QA One", "team-qa")

	// Engineer requests to join.
	r := post(t, eng, ts.URL+"/bookings/collab/request", url.Values{
		"id": {strconv.FormatInt(resID, 10)}, "team": {"team-eng"},
	})
	loc := r.Header.Get("Location")
	r.Body.Close()
	if !strings.Contains(loc, "notice=") {
		t.Fatalf("request failed: %s", loc)
	}
	c := collabFor(t, st, resID, "eng1")
	if c == nil || c.Status != store.CollabPending {
		t.Fatalf("expected pending request, got %+v", c)
	}

	// QA (owner) approves.
	r = post(t, qa, ts.URL+"/bookings/collab/decide", url.Values{
		"id": {strconv.FormatInt(c.ID, 10)}, "decision": {"approve"},
	})
	loc = r.Header.Get("Location")
	r.Body.Close()
	if !strings.Contains(loc, "notice=") {
		t.Fatalf("decide failed: %s", loc)
	}
	res, _ := st.ReservationByID(resID)
	engUser, err := st.UserByUsername("eng1")
	if err != nil || engUser == nil {
		t.Fatalf("lookup eng1: %v", err)
	}
	if ok, err := st.IsHolder(*res, engUser.ID); err != nil || !ok {
		t.Fatalf("engineer must be a holder after approval: ok=%v err=%v", ok, err)
	}
}

func TestCollab_OwnerCannotRequestOwnBooking(t *testing.T) {
	ts, st := newTestServer(t, "dev-01")
	qa := provisionAndLogin(t, ts, st, "qa1", store.RoleEngineering)
	resID := bookAs(t, qa, ts.URL, st, "dev-01", "QA One", "team-qa")

	r := post(t, qa, ts.URL+"/bookings/collab/request", url.Values{
		"id": {strconv.FormatInt(resID, 10)}, "team": {"team-qa"},
	})
	loc := r.Header.Get("Location")
	r.Body.Close()
	if !strings.Contains(loc, "error=") {
		t.Fatalf("owner requesting own booking should fail, got %s", loc)
	}
}

func TestCollab_SecondRequestRejectedWhileFirstActive(t *testing.T) {
	ts, st := newTestServer(t, "dev-01")
	qa := provisionAndLogin(t, ts, st, "qa1", store.RoleEngineering)
	eng1 := provisionAndLogin(t, ts, st, "eng1", store.RoleEngineering)
	eng2 := provisionAndLogin(t, ts, st, "eng2", store.RoleEngineering)
	resID := bookAs(t, qa, ts.URL, st, "dev-01", "QA One", "team-qa")

	r := post(t, eng1, ts.URL+"/bookings/collab/request", url.Values{"id": {strconv.FormatInt(resID, 10)}, "team": {"t"}})
	r.Body.Close()

	r = post(t, eng2, ts.URL+"/bookings/collab/request", url.Values{"id": {strconv.FormatInt(resID, 10)}, "team": {"t"}})
	loc := r.Header.Get("Location")
	r.Body.Close()
	if !strings.Contains(loc, "error=") {
		t.Fatalf("second concurrent request should be rejected, got %s", loc)
	}
}

func TestCollab_OnlyOwnerCanDecide(t *testing.T) {
	ts, st := newTestServer(t, "dev-01")
	qa := provisionAndLogin(t, ts, st, "qa1", store.RoleEngineering)
	eng := provisionAndLogin(t, ts, st, "eng1", store.RoleEngineering)
	stranger := provisionAndLogin(t, ts, st, "stranger", store.RoleEngineering)
	resID := bookAs(t, qa, ts.URL, st, "dev-01", "QA One", "team-qa")

	r := post(t, eng, ts.URL+"/bookings/collab/request", url.Values{"id": {strconv.FormatInt(resID, 10)}, "team": {"t"}})
	r.Body.Close()
	c := collabFor(t, st, resID, "eng1")

	r = post(t, stranger, ts.URL+"/bookings/collab/decide", url.Values{
		"id": {strconv.FormatInt(c.ID, 10)}, "decision": {"approve"},
	})
	defer r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner deciding: want 403, got %d", r.StatusCode)
	}
}

func TestCollab_RejectThenNewRequestAllowed(t *testing.T) {
	ts, st := newTestServer(t, "dev-01")
	qa := provisionAndLogin(t, ts, st, "qa1", store.RoleEngineering)
	eng1 := provisionAndLogin(t, ts, st, "eng1", store.RoleEngineering)
	eng2 := provisionAndLogin(t, ts, st, "eng2", store.RoleEngineering)
	resID := bookAs(t, qa, ts.URL, st, "dev-01", "QA One", "team-qa")

	r := post(t, eng1, ts.URL+"/bookings/collab/request", url.Values{"id": {strconv.FormatInt(resID, 10)}, "team": {"t"}})
	r.Body.Close()
	c := collabFor(t, st, resID, "eng1")

	r = post(t, qa, ts.URL+"/bookings/collab/decide", url.Values{
		"id": {strconv.FormatInt(c.ID, 10)}, "decision": {"reject"},
	})
	r.Body.Close()

	r = post(t, eng2, ts.URL+"/bookings/collab/request", url.Values{"id": {strconv.FormatInt(resID, 10)}, "team": {"t"}})
	loc := r.Header.Get("Location")
	r.Body.Close()
	if !strings.Contains(loc, "notice=") {
		t.Fatalf("new request after rejection should succeed, got %s", loc)
	}
}

func TestCollab_LeaveBySelfOnly(t *testing.T) {
	ts, st := newTestServer(t, "dev-01")
	qa := provisionAndLogin(t, ts, st, "qa1", store.RoleEngineering)
	eng := provisionAndLogin(t, ts, st, "eng1", store.RoleEngineering)
	stranger := provisionAndLogin(t, ts, st, "stranger", store.RoleEngineering)
	resID := bookAs(t, qa, ts.URL, st, "dev-01", "QA One", "team-qa")

	r := post(t, eng, ts.URL+"/bookings/collab/request", url.Values{"id": {strconv.FormatInt(resID, 10)}, "team": {"t"}})
	r.Body.Close()
	c := collabFor(t, st, resID, "eng1")
	r = post(t, qa, ts.URL+"/bookings/collab/decide", url.Values{"id": {strconv.FormatInt(c.ID, 10)}, "decision": {"approve"}})
	r.Body.Close()

	// A stranger cannot leave on someone else's behalf.
	r = post(t, stranger, ts.URL+"/bookings/collab/leave", url.Values{"id": {strconv.FormatInt(c.ID, 10)}})
	if r.StatusCode != http.StatusForbidden {
		r.Body.Close()
		t.Fatalf("stranger leave: want 403, got %d", r.StatusCode)
	}
	r.Body.Close()

	// The co-owner leaves successfully.
	r = post(t, eng, ts.URL+"/bookings/collab/leave", url.Values{"id": {strconv.FormatInt(c.ID, 10)}})
	loc := r.Header.Get("Location")
	r.Body.Close()
	if !strings.Contains(loc, "notice=") {
		t.Fatalf("leave failed: %s", loc)
	}
	res, _ := st.ReservationByID(resID)
	engUser, _ := st.UserByUsername("eng1")
	if ok, _ := st.IsHolder(*res, engUser.ID); ok {
		t.Fatal("after leaving, must no longer be a holder")
	}

	// Slot is open again for a new request.
	r = post(t, stranger, ts.URL+"/bookings/collab/request", url.Values{"id": {strconv.FormatInt(resID, 10)}, "team": {"t"}})
	loc = r.Header.Get("Location")
	r.Body.Close()
	if !strings.Contains(loc, "notice=") {
		t.Fatalf("request after leave should succeed, got %s", loc)
	}
}

func TestCollab_OwnerCanRevokeApprovedCoOwner(t *testing.T) {
	ts, st := newTestServer(t, "dev-01")
	qa := provisionAndLogin(t, ts, st, "qa1", store.RoleEngineering)
	eng := provisionAndLogin(t, ts, st, "eng1", store.RoleEngineering)
	resID := bookAs(t, qa, ts.URL, st, "dev-01", "QA One", "team-qa")

	r := post(t, eng, ts.URL+"/bookings/collab/request", url.Values{"id": {strconv.FormatInt(resID, 10)}, "team": {"t"}})
	r.Body.Close()
	c := collabFor(t, st, resID, "eng1")
	r = post(t, qa, ts.URL+"/bookings/collab/decide", url.Values{"id": {strconv.FormatInt(c.ID, 10)}, "decision": {"approve"}})
	r.Body.Close()

	r = post(t, qa, ts.URL+"/bookings/collab/revoke", url.Values{"id": {strconv.FormatInt(c.ID, 10)}})
	loc := r.Header.Get("Location")
	r.Body.Close()
	if !strings.Contains(loc, "notice=") {
		t.Fatalf("owner revoke failed: %s", loc)
	}
	res, _ := st.ReservationByID(resID)
	engUser, _ := st.UserByUsername("eng1")
	if ok, _ := st.IsHolder(*res, engUser.ID); ok {
		t.Fatal("revoked co-owner must no longer be a holder")
	}
}

func TestCollab_StrangerCannotRevoke(t *testing.T) {
	ts, st := newTestServer(t, "dev-01")
	qa := provisionAndLogin(t, ts, st, "qa1", store.RoleEngineering)
	eng := provisionAndLogin(t, ts, st, "eng1", store.RoleEngineering)
	stranger := provisionAndLogin(t, ts, st, "stranger", store.RoleEngineering)
	resID := bookAs(t, qa, ts.URL, st, "dev-01", "QA One", "team-qa")

	r := post(t, eng, ts.URL+"/bookings/collab/request", url.Values{"id": {strconv.FormatInt(resID, 10)}, "team": {"t"}})
	r.Body.Close()
	c := collabFor(t, st, resID, "eng1")
	r = post(t, qa, ts.URL+"/bookings/collab/decide", url.Values{"id": {strconv.FormatInt(c.ID, 10)}, "decision": {"approve"}})
	r.Body.Close()

	r = post(t, stranger, ts.URL+"/bookings/collab/revoke", url.Values{"id": {strconv.FormatInt(c.ID, 10)}})
	if r.StatusCode != http.StatusForbidden {
		r.Body.Close()
		t.Fatalf("stranger revoke: want 403, got %d", r.StatusCode)
	}
	r.Body.Close()
}

func TestCollab_AdminCanDecideAndRevoke(t *testing.T) {
	ts, st := newTestServer(t, "dev-01")
	qa := provisionAndLogin(t, ts, st, "qa1", store.RoleEngineering)
	eng := provisionAndLogin(t, ts, st, "eng1", store.RoleEngineering)
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	resID := bookAs(t, qa, ts.URL, st, "dev-01", "QA One", "team-qa")

	r := post(t, eng, ts.URL+"/bookings/collab/request", url.Values{"id": {strconv.FormatInt(resID, 10)}, "team": {"t"}})
	r.Body.Close()
	c := collabFor(t, st, resID, "eng1")

	// Admin, not the owner, can still approve.
	r = post(t, admin, ts.URL+"/bookings/collab/decide", url.Values{"id": {strconv.FormatInt(c.ID, 10)}, "decision": {"approve"}})
	loc := r.Header.Get("Location")
	r.Body.Close()
	if !strings.Contains(loc, "notice=") {
		t.Fatalf("admin decide failed: %s", loc)
	}

	// Admin can also revoke.
	r = post(t, admin, ts.URL+"/bookings/collab/revoke", url.Values{"id": {strconv.FormatInt(c.ID, 10)}})
	loc = r.Header.Get("Location")
	r.Body.Close()
	if !strings.Contains(loc, "notice=") {
		t.Fatalf("admin revoke failed: %s", loc)
	}
}

func TestCollab_BookingEndedRevokesAccessEvenIfRowStillApproved(t *testing.T) {
	ts, st := newTestServer(t, "dev-01")
	qa := provisionAndLogin(t, ts, st, "qa1", store.RoleEngineering)
	eng := provisionAndLogin(t, ts, st, "eng1", store.RoleEngineering)
	resID := bookAs(t, qa, ts.URL, st, "dev-01", "QA One", "team-qa")

	r := post(t, eng, ts.URL+"/bookings/collab/request", url.Values{"id": {strconv.FormatInt(resID, 10)}, "team": {"t"}})
	r.Body.Close()
	c := collabFor(t, st, resID, "eng1")
	r = post(t, qa, ts.URL+"/bookings/collab/decide", url.Values{"id": {strconv.FormatInt(c.ID, 10)}, "decision": {"approve"}})
	r.Body.Close()

	// Owner releases the booking via the real release endpoint.
	r = post(t, qa, ts.URL+"/bookings/release", url.Values{"id": {strconv.FormatInt(resID, 10)}})
	loc := r.Header.Get("Location")
	r.Body.Close()
	if !strings.Contains(loc, "notice=") {
		t.Fatalf("release failed: %s", loc)
	}

	res, _ := st.ReservationByID(resID)
	engUser, _ := st.UserByUsername("eng1")
	if ok, _ := st.IsHolder(*res, engUser.ID); ok {
		t.Fatal("co-owner must lose access once the booking is released, even though the collaborator row is untouched")
	}
}

func TestCollab_AuditTrail(t *testing.T) {
	ts, st := newTestServer(t, "dev-01")
	qa := provisionAndLogin(t, ts, st, "qa1", store.RoleEngineering)
	eng := provisionAndLogin(t, ts, st, "eng1", store.RoleEngineering)
	resID := bookAs(t, qa, ts.URL, st, "dev-01", "QA One", "team-qa")

	r := post(t, eng, ts.URL+"/bookings/collab/request", url.Values{"id": {strconv.FormatInt(resID, 10)}, "team": {"t"}})
	r.Body.Close()
	c := collabFor(t, st, resID, "eng1")
	r = post(t, qa, ts.URL+"/bookings/collab/decide", url.Values{"id": {strconv.FormatInt(c.ID, 10)}, "decision": {"approve"}})
	r.Body.Close()
	r = post(t, qa, ts.URL+"/bookings/collab/revoke", url.Values{"id": {strconv.FormatInt(c.ID, 10)}})
	r.Body.Close()

	entries, err := st.SearchAudit("COLLAB", 50)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, e := range entries {
		actions[e.Action] = true
	}
	for _, want := range []string{"REQUEST_COLLAB", "APPROVE_COLLAB", "REVOKE_COLLAB"} {
		if !actions[want] {
			t.Fatalf("missing audit action %s; got %+v", want, actions)
		}
	}
}
