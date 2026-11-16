package web

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"parkee/staging-platform/internal/auth"
	"parkee/staging-platform/internal/store"
)

func urlValue(s string) string { return url.QueryEscape(s) }

// bookingForm is the local timezone used to interpret datetime-local inputs.
var bookingLoc = time.Local

// maxBookingDays caps one booking so a typo (a year instead of a day) can't
// lock an env for months. Longer needs can be booked in several steps.
const maxBookingDays = 30

// bookedRange is one active booking, sent to the page so the calendar can
// mark taken days. Dates are local YYYY-MM-DD, both ends inclusive.
type bookedRange struct {
	Start   string `json:"s"`
	End     string `json:"e"`
	Name    string `json:"name"`
	Team    string `json:"team"`
	Purpose string `json:"purpose"`
	Mine    bool   `json:"mine"`
}

type bookingsPage struct {
	Username        string
	IsAdmin         bool
	StagingSlots    []string
	AutomationSlots []string
	Active          []store.Reservation
	Mine            []store.Reservation
	MaxDays         int
	Booked          map[string][]bookedRange // slot -> active bookings
	// Collab, keyed by reservation id, carries the co-ownership state needed
	// to render the right action (request/approve/reject/leave/revoke) for
	// the viewing user on that one row.
	Collab map[int64]collabView
	Error  string
	Notice string
}

// collabView is what the bookings page needs to know about one reservation's
// co-ownership, from the current viewer's point of view.
type collabView struct {
	// Approved is the active co-owner, if any (status == approved).
	Approved *store.Collaborator
	// PendingForOwner is a request the viewer (as the booking's owner) still
	// needs to approve/reject. Only set when the viewer owns this reservation.
	PendingForOwner *store.Collaborator
	// MyRequest is the viewer's own pending request on someone else's
	// booking, if any.
	MyRequest *store.Collaborator
	// OthersPending is true when some other user's request is pending on
	// this booking - visible to a third viewer as "a decision is pending",
	// without exposing who requested it.
	OthersPending bool
	// CanRequest is true when the viewer is neither the owner nor already
	// holding a pending/approved request, and the slot has room for one.
	CanRequest bool
}

// slotNames lists every bookable env: staging first, then automation.
func (s *Server) slotNames() []string {
	return append(s.envNames(store.ServerStaging), s.envNames(store.ServerAutomation)...)
}

func (s *Server) handleBookings(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())

	active, _ := s.st.ListActive()
	mine, _ := s.st.ListForUser(u.ID, 20)
	booked := map[string][]bookedRange{}
	for _, res := range active { // ListActive is ordered by slot, then start
		booked[res.Slot] = append(booked[res.Slot], bookedRange{
			Start:   res.Start.In(bookingLoc).Format("2006-01-02"),
			End:     res.End.In(bookingLoc).Format("2006-01-02"),
			Name:    res.Name,
			Team:    res.Team,
			Purpose: res.Purpose,
			Mine:    res.UserID == u.ID,
		})
	}
	s.renderBookings(w, bookingsPage{
		Username:        u.Username,
		IsAdmin:         u.IsAdmin(),
		StagingSlots:    s.envNames(store.ServerStaging),
		AutomationSlots: s.envNames(store.ServerAutomation),
		Active:          active,
		Mine:            mine,
		MaxDays:         maxBookingDays,
		Booked:          booked,
		Collab:          s.collabViewsFor(active, u.ID),
		Notice:          r.URL.Query().Get("notice"),
		Error:           r.URL.Query().Get("error"),
	})
}

// collabViewsFor builds the per-reservation co-ownership view for everything
// on the page, from viewerID's perspective.
//
// This must stay a single batch query (CollaboratorsForReservations) over
// all active reservations, not a loop calling the singular
// CollaboratorsForReservation per row: the bookings page can list many
// active bookings at once, and one query per row would be classic N+1 -
// linear round-trips to SQLite on every page load. Do not "simplify" this
// back into a per-reservation call.
//
// CollaboratorsForReservations only returns pending/approved rows (never
// rejected/revoked history), which is exactly what rendering needs here; it
// is a different, smaller batch than full per-reservation history would be.
func (s *Server) collabViewsFor(active []store.Reservation, viewerID int64) map[int64]collabView {
	ids := make([]int64, len(active))
	for i, res := range active {
		ids[i] = res.ID
	}
	byRes, err := s.st.CollaboratorsForReservations(ids)
	if err != nil {
		return map[int64]collabView{}
	}
	out := make(map[int64]collabView, len(active))
	for _, res := range active {
		list := byRes[res.ID]
		var v collabView
		hasActive := false // any pending or approved row at all
		for i := range list {
			c := list[i]
			switch c.Status {
			case store.CollabApproved:
				hasActive = true
				v.Approved = &c
			case store.CollabPending:
				hasActive = true
				if res.UserID == viewerID {
					v.PendingForOwner = &c
				} else if c.UserID == viewerID {
					v.MyRequest = &c
				} else {
					v.OthersPending = true
				}
			}
		}
		// hasActive already covers "already approved" (viewer or anyone
		// else) and "already pending" (viewer or anyone else): only one
		// co-owner slot exists per booking, so any active row blocks a new
		// request regardless of who it belongs to.
		v.CanRequest = res.UserID != viewerID && !hasActive
		out[res.ID] = v
	}
	return out
}

func (s *Server) handleBookingCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/bookings", http.StatusSeeOther)
		return
	}
	u := auth.UserFrom(r.Context())
	slot := r.FormValue("slot")
	name := strings.TrimSpace(r.FormValue("name"))
	team := strings.TrimSpace(r.FormValue("team"))
	purpose := strings.TrimSpace(r.FormValue("purpose"))
	if name == "" {
		redirectBookings(w, r, "", "name is required")
		return
	}
	if team == "" {
		redirectBookings(w, r, "", "team is required")
		return
	}
	if purpose == "" {
		redirectBookings(w, r, "", "purpose is required")
		return
	}
	if len(name) > 120 || len(team) > 120 || len(purpose) > 200 {
		redirectBookings(w, r, "", "name and team must be at most 120 characters; purpose must be at most 200 characters")
		return
	}
	startDate, endDate := r.FormValue("startDate"), r.FormValue("endDate")
	start, end, err := parseBookingRange(startDate, endDate)
	if err != nil {
		redirectBookings(w, r, "", err.Error())
		return
	}
	if slot == "" {
		redirectBookings(w, r, "", "slot is required")
		return
	}

	res, err := s.st.CreateReservationWithDetails(slot, u.ID, u.Username, name, team, purpose, start, end)
	if err != nil {
		var conflict *store.ConflictError
		if errors.As(err, &conflict) {
			by := conflict.Name
			if by == "" {
				by = conflict.Username
			}
			if conflict.Team != "" {
				by += " (" + conflict.Team + ")"
			}
			redirectBookings(w, r, "", "That slot is already booked by "+by+
				" until "+conflict.Until.Local().Format("2006-01-02 15:04")+
				" — reach out to them if you need it sooner.")
			return
		}
		redirectBookings(w, r, "", err.Error())
		return
	}
	_ = s.st.Audit(u.ID, u.Username, "RESERVE", "reservation", strconv.FormatInt(res.ID, 10),
		map[string]any{"slot": slot, "start": start, "end": end, "name": name, "team": team, "purpose": purpose})
	when := startDate
	if endDate != "" && endDate != startDate {
		when += " to " + endDate
	}
	redirectBookings(w, r, "Booked "+slot+" for "+when+".", "")
}

func (s *Server) handleBookingRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/bookings", http.StatusSeeOther)
		return
	}
	u := auth.UserFrom(r.Context())
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	res, err := s.st.ReservationByID(id)
	if err != nil || res == nil {
		redirectBookings(w, r, "", "reservation not found")
		return
	}

	// Owner may release; admin may override someone else's booking.
	isOwner := res.UserID == u.ID
	if !isOwner && !u.IsAdmin() {
		http.Error(w, "forbidden: not your reservation", http.StatusForbidden)
		return
	}
	status := store.ResReleased
	action := "RELEASE"
	if !isOwner {
		status = store.ResOverridden
		action = "OVERRIDE_RELEASE"
	}
	if err := s.st.Release(id, status); err != nil {
		redirectBookings(w, r, "", err.Error())
		return
	}
	_ = s.st.Audit(u.ID, u.Username, action, "reservation", strconv.FormatInt(id, 10),
		map[string]any{"slot": res.Slot, "owner": res.Username})
	redirectBookings(w, r, "Released booking for "+res.Slot+".", "")
}

// handleCollabRequest lets a non-owner ask to become the co-owner of an
// active booking. Approval is up to the booking's owner (handleCollabDecide).
func (s *Server) handleCollabRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/bookings", http.StatusSeeOther)
		return
	}
	u := auth.UserFrom(r.Context())
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	team := strings.TrimSpace(r.FormValue("team"))
	if team == "" {
		redirectBookings(w, r, "", "team is required")
		return
	}
	if len(team) > 120 {
		redirectBookings(w, r, "", "team must be at most 120 characters")
		return
	}
	res, err := s.st.ReservationByID(id)
	if err != nil || res == nil {
		redirectBookings(w, r, "", "reservation not found")
		return
	}
	c, err := s.st.RequestCollaborator(res.ID, u.ID, u.Username, u.Name, team)
	if err != nil {
		redirectBookings(w, r, "", err.Error())
		return
	}
	_ = s.st.Audit(u.ID, u.Username, "REQUEST_COLLAB", "reservation", strconv.FormatInt(res.ID, 10),
		map[string]any{"slot": res.Slot, "owner": res.Username, "collabID": c.ID})
	redirectBookings(w, r, "Requested to join "+res.Slot+"; waiting for "+bookingDisplayName(*res)+" to approve.", "")
}

// handleCollabDecide lets a booking's owner approve or reject a pending
// co-owner request on their own booking.
func (s *Server) handleCollabDecide(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/bookings", http.StatusSeeOther)
		return
	}
	u := auth.UserFrom(r.Context())
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	approve := r.FormValue("decision") == "approve"

	c, err := s.st.CollaboratorByID(id)
	if err != nil || c == nil {
		redirectBookings(w, r, "", "co-owner request not found")
		return
	}
	res, err := s.st.ReservationByID(c.ReservationID)
	if err != nil || res == nil {
		redirectBookings(w, r, "", "reservation not found")
		return
	}
	if res.UserID != u.ID && !u.IsAdmin() {
		http.Error(w, "forbidden: not your reservation", http.StatusForbidden)
		return
	}
	if err := s.st.DecideCollaborator(c.ID, approve, u.ID); err != nil {
		redirectBookings(w, r, "", err.Error())
		return
	}
	action, verb := "REJECT_COLLAB", "Rejected"
	if approve {
		action, verb = "APPROVE_COLLAB", "Approved"
	}
	_ = s.st.Audit(u.ID, u.Username, action, "reservation", strconv.FormatInt(res.ID, 10),
		map[string]any{"slot": res.Slot, "collabID": c.ID, "requester": c.Username})
	redirectBookings(w, r, verb+" "+c.Name+" as co-owner of "+res.Slot+".", "")
}

// handleCollabLeave lets an approved co-owner give up their own access,
// without touching the booking itself. It calls the same store.RevokeCollaborator
// as handleCollabRevoke (owner/admin removing someone else) - the row ends
// up with the same status='revoked' either way. The only place the two are
// distinguished is the audit action below (LEAVE_COLLAB vs REVOKE_COLLAB);
// see the comment on store.RevokeCollaborator for why that is intentional.
func (s *Server) handleCollabLeave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/bookings", http.StatusSeeOther)
		return
	}
	u := auth.UserFrom(r.Context())
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	c, err := s.st.CollaboratorByID(id)
	if err != nil || c == nil {
		redirectBookings(w, r, "", "co-owner request not found")
		return
	}
	if c.UserID != u.ID {
		http.Error(w, "forbidden: not your co-owner request", http.StatusForbidden)
		return
	}
	if err := s.st.RevokeCollaborator(c.ID); err != nil {
		redirectBookings(w, r, "", err.Error())
		return
	}
	res, _ := s.st.ReservationByID(c.ReservationID)
	slotName := ""
	if res != nil {
		slotName = res.Slot
	}
	_ = s.st.Audit(u.ID, u.Username, "LEAVE_COLLAB", "reservation", strconv.FormatInt(c.ReservationID, 10),
		map[string]any{"slot": slotName})
	redirectBookings(w, r, "You left the co-owner role for "+slotName+".", "")
}

// handleCollabRevoke lets a booking's owner (or an admin) remove an approved
// co-owner.
func (s *Server) handleCollabRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/bookings", http.StatusSeeOther)
		return
	}
	u := auth.UserFrom(r.Context())
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	c, err := s.st.CollaboratorByID(id)
	if err != nil || c == nil {
		redirectBookings(w, r, "", "co-owner request not found")
		return
	}
	res, err := s.st.ReservationByID(c.ReservationID)
	if err != nil || res == nil {
		redirectBookings(w, r, "", "reservation not found")
		return
	}
	if res.UserID != u.ID && !u.IsAdmin() {
		http.Error(w, "forbidden: not your reservation", http.StatusForbidden)
		return
	}
	if err := s.st.RevokeCollaborator(c.ID); err != nil {
		redirectBookings(w, r, "", err.Error())
		return
	}
	_ = s.st.Audit(u.ID, u.Username, "REVOKE_COLLAB", "reservation", strconv.FormatInt(res.ID, 10),
		map[string]any{"slot": res.Slot, "revoked": c.Username})
	redirectBookings(w, r, "Removed "+c.Name+" as co-owner of "+res.Slot+".", "")
}

// parseBookingRange reads a start date and an optional end date (YYYY-MM-DD,
// both inclusive) as a whole-day booking in local time: start = 00:00:00 of
// the first day, end = 23:59:59 of the last day. A blank end date means a
// single day.
func parseBookingRange(startStr, endStr string) (time.Time, time.Time, error) {
	if startStr == "" {
		return time.Time{}, time.Time{}, errors.New("start date is required")
	}
	const layout = "2006-01-02"
	first, err := time.ParseInLocation(layout, startStr, bookingLoc)
	if err != nil {
		return time.Time{}, time.Time{}, errors.New("invalid start date")
	}
	last := first
	if endStr != "" {
		if last, err = time.ParseInLocation(layout, endStr, bookingLoc); err != nil {
			return time.Time{}, time.Time{}, errors.New("invalid end date")
		}
	}
	if last.Before(first) {
		return time.Time{}, time.Time{}, errors.New("end date is before the start date")
	}
	now := time.Now().In(bookingLoc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, bookingLoc)
	if first.Before(today) {
		return time.Time{}, time.Time{}, errors.New("start date is in the past")
	}
	days := int(math.Round(last.Sub(first).Hours()/24)) + 1
	if days > maxBookingDays {
		return time.Time{}, time.Time{}, fmt.Errorf("a booking can be at most %d days (you asked for %d)", maxBookingDays, days)
	}
	end := last.AddDate(0, 0, 1).Add(-time.Second)
	return first, end, nil
}

func redirectBookings(w http.ResponseWriter, r *http.Request, notice, errMsg string) {
	u := "/bookings"
	q := ""
	if notice != "" {
		q = "?notice=" + urlValue(notice)
	} else if errMsg != "" {
		q = "?error=" + urlValue(errMsg)
	}
	http.Redirect(w, r, u+q, http.StatusSeeOther)
}
