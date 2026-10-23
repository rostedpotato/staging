package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"parkee/staging-platform/internal/auth"
	"parkee/staging-platform/internal/store"
)

// Machine-facing JSON API, authenticated with static API keys (see
// auth.RequireAPIKey). Everything lives under /api/v1; a breaking change would
// go to /api/v2, while adding fields or endpoints stays in v1.

// Scopes understood by /api/v1. A new endpoint adds a scope here.
const scopeBookingsRead = "bookings:read"

const (
	apiDefaultLimit  = 100
	apiMaxLimit      = 500
	apiMaxRangeDays  = 366
	apiDateLayout    = "2006-01-02"
	apiErrBadRequest = "invalid_request"
)

var apiBookingStatuses = map[string]bool{
	store.ResActive: true, store.ResReleased: true,
	store.ResExpired: true, store.ResOverridden: true,
}

// apiBooking is the public shape of a booking. It is deliberately separate
// from store.Reservation so internal fields (e.g. the user id) never leak and
// the contract does not change when the storage struct does. Times are
// RFC3339 in the booking timezone (WIB in production).
type apiBooking struct {
	ID       int64  `json:"id"`
	Slot     string `json:"slot"`
	Username string `json:"username"`
	Purpose  string `json:"purpose"`
	// Start/End are the planned window; EffectiveEnd is when the booking
	// really stopped holding the env (earlier than End if released early).
	Start        time.Time  `json:"start"`
	End          time.Time  `json:"end"`
	EffectiveEnd time.Time  `json:"effectiveEnd"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"createdAt"`
	ReleasedAt   *time.Time `json:"releasedAt"`
}

func toAPIBooking(r store.Reservation) apiBooking {
	b := apiBooking{
		ID: r.ID, Slot: r.Slot, Username: r.Username, Purpose: r.Purpose,
		Start:        r.Start.In(bookingLoc),
		End:          r.End.In(bookingLoc),
		EffectiveEnd: r.EffectiveEnd().In(bookingLoc),
		Status:       r.Status,
		CreatedAt:    r.CreatedTime().In(bookingLoc),
	}
	if !r.ReleasedAt.IsZero() {
		t := r.ReleasedAt.In(bookingLoc)
		b.ReleasedAt = &t
	}
	return b
}

type apiBookingsMeta struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Limit  int       `json:"limit"`
	Offset int       `json:"offset"`
	Total  int       `json:"total"`
}

type apiBookingsResponse struct {
	Data []apiBooking    `json:"data"`
	Meta apiBookingsMeta `json:"meta"`
}

// apiEndpoint declares one /api/v1 operation. The table is the single place
// to add an endpoint, and a test checks it against apidocs/openapi.yaml so
// the documentation cannot silently drift from the code.
type apiEndpoint struct {
	Method  string // documented method (the handler also answers HEAD for GET)
	Path    string
	Scope   string
	Handler http.HandlerFunc
}

func (s *Server) apiEndpoints() []apiEndpoint {
	return []apiEndpoint{
		{http.MethodGet, "/api/v1/bookings", scopeBookingsRead, s.handleAPIBookings},
	}
}

// apiRoutes registers /api/v1. Add new endpoints to apiEndpoints (and to
// apidocs/openapi.yaml, which the test suite enforces).
func (s *Server) apiRoutes(mux *http.ServeMux) {
	for _, e := range s.apiEndpoints() {
		mux.Handle(e.Path, auth.RequireAPIKey(s.apiKeys, e.Scope, e.Handler))
	}
	// Unknown /api/v1 paths must answer JSON, not fall through to the HTML
	// login redirect of "/".
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		auth.WriteAPIError(w, http.StatusNotFound, "not_found", "unknown endpoint")
	})
}

// handleAPIBookings serves GET /api/v1/bookings: bookings whose window
// overlaps [from, to), optionally filtered by slot and status, paginated.
func (s *Server) handleAPIBookings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		auth.WriteAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	q := r.URL.Query()
	bad := func(msg string) {
		auth.WriteAPIError(w, http.StatusBadRequest, apiErrBadRequest, msg)
	}

	from, err := parseAPITime(q.Get("from"), false)
	if err != nil {
		bad("from: " + err.Error())
		return
	}
	to, err := parseAPITime(q.Get("to"), true)
	if err != nil {
		bad("to: " + err.Error())
		return
	}
	if !to.After(from) {
		bad("to must be after from")
		return
	}
	if to.After(from.AddDate(0, 0, apiMaxRangeDays)) {
		bad(fmt.Sprintf("range too large: at most %d days between from and to", apiMaxRangeDays))
		return
	}

	status := q.Get("status")
	if status != "" && !apiBookingStatuses[status] {
		bad("status must be one of: active, released, expired, overridden")
		return
	}
	limit, err := parseAPIInt(q.Get("limit"), apiDefaultLimit, 1, apiMaxLimit)
	if err != nil {
		bad("limit: " + err.Error())
		return
	}
	offset, err := parseAPIInt(q.Get("offset"), 0, 0, int(^uint(0)>>1))
	if err != nil {
		bad("offset: " + err.Error())
		return
	}

	// Overlap semantics (same as the dashboard): start < to AND end > from.
	rows, err := s.st.ListHistory(from, to, q.Get("slot"), "date_asc")
	if err != nil {
		log.Printf("api: list bookings: %v", err)
		auth.WriteAPIError(w, http.StatusInternalServerError, "internal", "could not load bookings")
		return
	}
	// Stable order so offset pagination never skips or repeats a row.
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].Start.Equal(rows[j].Start) {
			return rows[i].Start.Before(rows[j].Start)
		}
		return rows[i].ID < rows[j].ID
	})
	if status != "" {
		kept := rows[:0]
		for _, row := range rows {
			if row.Status == status {
				kept = append(kept, row)
			}
		}
		rows = kept
	}

	total := len(rows)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	data := make([]apiBooking, 0, end-offset)
	for _, row := range rows[offset:end] {
		data = append(data, toAPIBooking(row))
	}

	if p := auth.PrincipalFrom(r.Context()); p != nil {
		log.Printf("api: key=%q GET /api/v1/bookings from=%s to=%s -> %d/%d",
			p.Name, from.Format(time.RFC3339), to.Format(time.RFC3339), len(data), total)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(apiBookingsResponse{
		Data: data,
		Meta: apiBookingsMeta{
			From: from.In(bookingLoc), To: to.In(bookingLoc),
			Limit: limit, Offset: offset, Total: total,
		},
	})
}

// parseAPITime accepts "YYYY-MM-DD" (a whole day in the booking timezone) or
// RFC3339. For a date-only upper bound (isTo) the whole day is included, i.e.
// the bound becomes the start of the next day.
func parseAPITime(s string, isTo bool) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("required")
	}
	// A "+" in a query string decodes to a space, which silently breaks
	// offsets like "+07:00" when clients forget to percent-encode.
	s = strings.ReplaceAll(s, " ", "+")
	if len(s) == len(apiDateLayout) {
		d, err := time.ParseInLocation(apiDateLayout, s, bookingLoc)
		if err != nil {
			return time.Time{}, errors.New("expected YYYY-MM-DD or RFC3339")
		}
		if isTo {
			d = d.AddDate(0, 0, 1)
		}
		return d, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, errors.New("expected YYYY-MM-DD or RFC3339")
	}
	return t, nil
}

func parseAPIInt(s string, def, min, max int) (int, error) {
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, errors.New("must be an integer")
	}
	if n < min || n > max {
		if max == int(^uint(0)>>1) {
			return 0, fmt.Errorf("must be >= %d", min)
		}
		return 0, fmt.Errorf("must be between %d and %d", min, max)
	}
	return n, nil
}
