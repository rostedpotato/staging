package web

import (
	"context"
	"errors"
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

type bookingsPage struct {
	Username string
	IsAdmin  bool
	Slots    []string
	Active   []store.Reservation
	Mine     []store.Reservation
	Error    string
	Notice   string
}

func (s *Server) slotNames(ctx context.Context) []string {
	slots, _ := s.scan(ctx)
	names := make([]string, 0, len(slots))
	for _, sl := range slots {
		names = append(names, sl.Name)
	}
	return names
}

func (s *Server) handleBookings(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	u := auth.UserFrom(r.Context())

	active, _ := s.st.ListActive()
	mine, _ := s.st.ListForUser(u.ID, 20)
	s.renderBookings(w, bookingsPage{
		Username: u.Username,
		IsAdmin:  u.IsAdmin(),
		Slots:    s.slotNames(ctx),
		Active:   active,
		Mine:     mine,
		Notice:   r.URL.Query().Get("notice"),
		Error:    r.URL.Query().Get("error"),
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
	start, end, err := parseDateRange(r.FormValue("startDate"), r.FormValue("endDate"))
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
		if errors.Is(err, store.ErrConflict) {
			redirectBookings(w, r, "", "That slot is already booked for the selected window.")
			return
		}
		redirectBookings(w, r, "", err.Error())
		return
	}
	_ = s.st.Audit(u.ID, u.Username, "RESERVE", "reservation", strconv.FormatInt(res.ID, 10),
		map[string]any{"slot": slot, "start": start, "end": end, "purpose": purpose})
	redirectBookings(w, r, "Booked "+slot+".", "")
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

// parseDateRange reads two date values (YYYY-MM-DD, no time) as whole-day
// bookings in local time: start = 00:00:00 of startDate, end = 23:59:59 of
// endDate. A single-day booking has startDate == endDate. If endDate is blank
// it defaults to startDate (book just that one day).
func parseDateRange(startStr, endStr string) (time.Time, time.Time, error) {
	if startStr == "" {
		return time.Time{}, time.Time{}, errors.New("date is required")
	}
	if endStr == "" {
		endStr = startStr
	}
	const layout = "2006-01-02"
	sd, err := time.ParseInLocation(layout, startStr, bookingLoc)
	if err != nil {
		return time.Time{}, time.Time{}, errors.New("invalid start date")
	}
	ed, err := time.ParseInLocation(layout, endStr, bookingLoc)
	if err != nil {
		return time.Time{}, time.Time{}, errors.New("invalid end date")
	}
	// Whole-day span: 00:00:00 of start .. 23:59:59 of end.
	start := sd
	end := ed.Add(24*time.Hour - time.Second)
	if end.Before(start) {
		return time.Time{}, time.Time{}, errors.New("end date must not be before start date")
	}
	// Reject a booking whose whole span is already in the past (yesterday).
	if end.Before(time.Now()) {
		return time.Time{}, time.Time{}, errors.New("that date is already in the past")
	}
	return start, end, nil
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
