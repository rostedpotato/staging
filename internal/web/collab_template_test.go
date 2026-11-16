package web

import (
	"net/url"
	"strconv"
	"strings"
	"testing"

	"parkee/staging-platform/internal/store"
)

// These tests exercise the rendered /bookings HTML for each co-ownership
// state, from every viewer's point of view - complementing collab_test.go
// (which checks the handlers/redirects) with what the page actually shows.

func TestCollabTemplate_CanRequestShownToStranger(t *testing.T) {
	ts, st := newTestServer(t, "dev-01")
	qa := provisionAndLogin(t, ts, st, "qa1", store.RoleEngineering)
	eng := provisionAndLogin(t, ts, st, "eng1", store.RoleEngineering)
	bookAs(t, qa, ts.URL, st, "dev-01", "QA One", "team-qa")

	page := body(t, eng, ts.URL+"/bookings")
	if !strings.Contains(page, "Request to join") {
		t.Fatalf("stranger should see a request form: %s", page)
	}
	if !strings.Contains(page, `action="/bookings/collab/request"`) {
		t.Fatal("request form action missing")
	}
	// Owner must not see a request-to-join form on their own booking.
	ownerPage := body(t, qa, ts.URL+"/bookings")
	if strings.Contains(ownerPage, "Request to join") {
		t.Fatalf("owner must not be offered a request form on their own booking: %s", ownerPage)
	}
}

func TestCollabTemplate_PendingShowsApproveRejectToOwnerOnly(t *testing.T) {
	ts, st := newTestServer(t, "dev-01")
	qa := provisionAndLogin(t, ts, st, "qa1", store.RoleEngineering)
	eng := provisionAndLogin(t, ts, st, "eng1", store.RoleEngineering)
	stranger := provisionAndLogin(t, ts, st, "stranger", store.RoleEngineering)
	resID := bookAs(t, qa, ts.URL, st, "dev-01", "QA One", "team-qa")

	r := post(t, eng, ts.URL+"/bookings/collab/request", url.Values{"id": {strconv.FormatInt(resID, 10)}, "team": {"team-eng"}})
	r.Body.Close()

	ownerPage := body(t, qa, ts.URL+"/bookings")
	if !strings.Contains(ownerPage, "Approve") || !strings.Contains(ownerPage, "Reject") {
		t.Fatalf("owner should see Approve/Reject: %s", ownerPage)
	}
	if !strings.Contains(ownerPage, "wants to join") {
		t.Fatal("owner page should name the requester")
	}

	// The requester sees a waiting state, not approve/reject buttons.
	engPage := body(t, eng, ts.URL+"/bookings")
	if strings.Contains(engPage, "Approve") {
		t.Fatalf("requester must not see Approve on their own pending request: %s", engPage)
	}
	if !strings.Contains(engPage, "Waiting for approval") {
		t.Fatalf("requester should see a waiting state: %s", engPage)
	}

	// A third party sees only that something is pending, no names, no form.
	strangerPage := body(t, stranger, ts.URL+"/bookings")
	if strings.Contains(strangerPage, "Request to join") {
		t.Fatal("stranger must not be offered a request form while one is already pending")
	}
	if !strings.Contains(strangerPage, "Request pending") {
		t.Fatalf("stranger should see a generic pending state: %s", strangerPage)
	}
	if strings.Contains(strangerPage, "wants to join") {
		t.Fatal("stranger must not see who is pending (that's owner-only)")
	}
}

func TestCollabTemplate_ApprovedShowsLeaveToCoOwnerAndRevokeToOwner(t *testing.T) {
	ts, st := newTestServer(t, "dev-01")
	qa := provisionAndLogin(t, ts, st, "qa1", store.RoleEngineering)
	eng := provisionAndLogin(t, ts, st, "eng1", store.RoleEngineering)
	stranger := provisionAndLogin(t, ts, st, "stranger", store.RoleEngineering)
	resID := bookAs(t, qa, ts.URL, st, "dev-01", "QA One", "team-qa")

	r := post(t, eng, ts.URL+"/bookings/collab/request", url.Values{"id": {strconv.FormatInt(resID, 10)}, "team": {"team-eng"}})
	r.Body.Close()
	c := collabFor(t, st, resID, "eng1")
	r = post(t, qa, ts.URL+"/bookings/collab/decide", url.Values{"id": {strconv.FormatInt(c.ID, 10)}, "decision": {"approve"}})
	r.Body.Close()

	// Co-owner sees their name and a Leave button.
	engPage := body(t, eng, ts.URL+"/bookings")
	if !strings.Contains(engPage, "Leave") {
		t.Fatalf("co-owner should see Leave: %s", engPage)
	}
	if strings.Contains(engPage, "Revoke") {
		t.Fatal("co-owner must not see Revoke (that's for the owner/admin)")
	}

	// Owner sees the co-owner's name and a Revoke button, not Leave.
	ownerPage := body(t, qa, ts.URL+"/bookings")
	if !strings.Contains(ownerPage, "Revoke") {
		t.Fatalf("owner should see Revoke: %s", ownerPage)
	}
	if !strings.Contains(ownerPage, "eng1 (team-eng)") { // co-owner's display name
		t.Fatalf("owner should see the co-owner's name: %s", ownerPage)
	}

	// A stranger just sees who the co-owner is, with no action available.
	strangerPage := body(t, stranger, ts.URL+"/bookings")
	if strings.Contains(strangerPage, "Leave") || strings.Contains(strangerPage, "Revoke") {
		t.Fatalf("stranger must not see Leave/Revoke: %s", strangerPage)
	}
	if !strings.Contains(strangerPage, "eng1 (team-eng)") {
		t.Fatal("stranger should still see who the co-owner is (public info)")
	}
}

func TestCollabTemplate_SlotFreeAgainAfterLeave(t *testing.T) {
	ts, st := newTestServer(t, "dev-01")
	qa := provisionAndLogin(t, ts, st, "qa1", store.RoleEngineering)
	eng := provisionAndLogin(t, ts, st, "eng1", store.RoleEngineering)
	resID := bookAs(t, qa, ts.URL, st, "dev-01", "QA One", "team-qa")

	r := post(t, eng, ts.URL+"/bookings/collab/request", url.Values{"id": {strconv.FormatInt(resID, 10)}, "team": {"team-eng"}})
	r.Body.Close()
	c := collabFor(t, st, resID, "eng1")
	r = post(t, qa, ts.URL+"/bookings/collab/decide", url.Values{"id": {strconv.FormatInt(c.ID, 10)}, "decision": {"approve"}})
	r.Body.Close()
	r = post(t, eng, ts.URL+"/bookings/collab/leave", url.Values{"id": {strconv.FormatInt(c.ID, 10)}})
	r.Body.Close()

	page := body(t, eng, ts.URL+"/bookings")
	if !strings.Contains(page, "Request to join") {
		t.Fatalf("after leaving, a request form should be offered again: %s", page)
	}
}
