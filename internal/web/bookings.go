package web

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
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
	User    string `json:"user"`
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
	Error           string
	Notice          string
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
			User:    res.Username,
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
		Notice:          r.URL.Query().Get("notice"),
		Error:           r.URL.Query().Get("error"),
	})
}

func (s *Server) handleBookingCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/bookings", http.StatusSeeOther)
		return
	}
	u := auth.UserFrom(r.Context())
	slot := r.FormValue("slot")
	purpose := r.FormValue("purpose")
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

	res, err := s.st.CreateReservation(slot, u.ID, u.Username, purpose, start, end)
	if err != nil {
		var conflict *store.ConflictError
		if errors.As(err, &conflict) {
			redirectBookings(w, r, "", "That slot is already booked by "+conflict.Username+
				" until "+conflict.Until.Local().Format("2006-01-02 15:04")+
				" — reach out to them if you need it sooner.")
			return
		}
		redirectBookings(w, r, "", err.Error())
		return
	}
	_ = s.st.Audit(u.ID, u.Username, "RESERVE", "reservation", strconv.FormatInt(res.ID, 10),
		map[string]any{"slot": slot, "start": start, "end": end, "purpose": purpose})
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
