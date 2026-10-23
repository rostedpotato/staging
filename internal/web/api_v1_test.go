package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"parkee/staging-platform/internal/auth"
	"parkee/staging-platform/internal/config"
	"parkee/staging-platform/internal/store"
)

const (
	apiTestKey      = "test-key-bi"
	apiTestOtherKey = "test-key-deploy-only"
)

func apiTestServer(t *testing.T, slots ...string) (string, *store.Store) {
	t.Helper()
	ts, st := newTestServerWith(t, func(tc *testConfig) {
		tc.apiKeys = []config.APIKey{
			{Name: "bi", Hash: auth.HashAPIKey(apiTestKey), Scopes: []string{scopeBookingsRead}},
			{Name: "deployer", Hash: auth.HashAPIKey(apiTestOtherKey), Scopes: []string{"deploy:write"}},
		}
	}, slots...)
	return ts.URL, st
}

func apiGet(t *testing.T, base, path, key string) (int, []byte, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	// Never follow redirects: an API must answer in place.
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func decodeBookings(t *testing.T, b []byte) apiBookingsResponse {
	t.Helper()
	var out apiBookingsResponse
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b)
	}
	return out
}

func decodeAPIErr(t *testing.T, b []byte) (code string) {
	t.Helper()
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(b, &e); err != nil || e.Error.Code == "" {
		t.Fatalf("not an API error body: %v\n%s", err, b)
	}
	return e.Error.Code
}

// apiDay returns midnight (booking tz) of the given date.
func apiDay(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, bookingLoc)
}

func mustBook(t *testing.T, st *store.Store, slot, user, purpose string, start, end time.Time) *store.Reservation {
	t.Helper()
	// reservations.user_id has a foreign key; reuse the user if it exists.
	var uid int64
	if u, err := st.UserByUsername(user); err == nil && u != nil {
		uid = u.ID
	} else {
		u, _ := provisionUser(t, st, user, store.RoleEngineering)
		uid = u.ID
	}
	r, err := st.CreateReservation(slot, uid, user, purpose, start, end)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAPIBookings_Auth(t *testing.T) {
	base, _ := apiTestServer(t, "dev-01")
	path := "/api/v1/bookings?from=2030-01-01&to=2030-01-31"

	cases := []struct {
		name string
		key  string
		want int
		code string
	}{
		{"no key", "", 401, "unauthorized"},
		{"wrong key", "nope", 401, "unauthorized"},
		{"key without scope", apiTestOtherKey, 403, "forbidden"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body, hdr := apiGet(t, base, path, c.key)
			if status != c.want {
				t.Fatalf("status %d, want %d (%s)", status, c.want, body)
			}
			if got := decodeAPIErr(t, body); got != c.code {
				t.Fatalf("code %q, want %q", got, c.code)
			}
			if hdr.Get("Location") != "" {
				t.Fatal("API must not redirect to /login")
			}
		})
	}
	if status, _, _ := apiGet(t, base, path, apiTestKey); status != 200 {
		t.Fatalf("valid key: status %d", status)
	}
}

func TestAPIBookings_SessionCookieNotAccepted(t *testing.T) {
	ts, st := newTestServer(t, "dev-01")
	u, temp := provisionUser(t, st, "alice", store.RoleEngineering)
	c := login(t, ts, u.Username, temp)
	resp, err := c.Get(ts.URL + "/api/v1/bookings?from=2030-01-01&to=2030-01-31")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("logged-in browser session must not open /api/v1; got %d", resp.StatusCode)
	}
}

func TestAPIBookings_UnknownPathAndMethod(t *testing.T) {
	base, _ := apiTestServer(t)
	status, body, _ := apiGet(t, base, "/api/v1/nope", apiTestKey)
	if status != 404 || decodeAPIErr(t, body) != "not_found" {
		t.Fatalf("got %d %s", status, body)
	}
	req, _ := http.NewRequest("POST", base+"/api/v1/bookings?from=2030-01-01&to=2030-01-02", nil)
	req.Header.Set("Authorization", "Bearer "+apiTestKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Fatalf("POST: got %d, want 405", resp.StatusCode)
	}
}

func TestAPIBookings_Validation(t *testing.T) {
	base, _ := apiTestServer(t)
	bad := []string{
		"",                               // nothing
		"?from=2030-01-01",               // missing to
		"?to=2030-01-31",                 // missing from
		"?from=garbage&to=2030-01-31",    // bad from
		"?from=2030-01-01&to=31-01-2030", // bad to
		"?from=2030-02-01&to=2030-01-01", // to before from
		"?from=2030-01-01&to=2032-01-01", // range too large
		"?from=2030-01-01&to=2030-01-31&status=bogus",
		"?from=2030-01-01&to=2030-01-31&limit=0",
		"?from=2030-01-01&to=2030-01-31&limit=501",
		"?from=2030-01-01&to=2030-01-31&limit=abc",
		"?from=2030-01-01&to=2030-01-31&offset=-1",
	}
	for _, q := range bad {
		status, body, _ := apiGet(t, base, "/api/v1/bookings"+q, apiTestKey)
		if status != 400 {
			t.Errorf("%q: status %d, want 400", q, status)
			continue
		}
		if decodeAPIErr(t, body) != apiErrBadRequest {
			t.Errorf("%q: wrong error code: %s", q, body)
		}
	}
}

func TestAPIBookings_RangeOverlapAndInclusiveTo(t *testing.T) {
	base, st := apiTestServer(t, "dev-01", "dev-02")
	// Whole-day bookings, booking tz.
	mustBook(t, st, "dev-01", "ana", "before", apiDay(2031, 3, 1), apiDay(2031, 3, 5))          // ends before range
	mustBook(t, st, "dev-01", "bob", "straddles-from", apiDay(2031, 3, 8), apiDay(2031, 3, 12)) // crosses from
	mustBook(t, st, "dev-02", "cat", "inside", apiDay(2031, 3, 12), apiDay(2031, 3, 14))
	mustBook(t, st, "dev-01", "dan", "last-day", apiDay(2031, 3, 20), apiDay(2031, 3, 21)) // starts on the `to` date
	mustBook(t, st, "dev-02", "eve", "after", apiDay(2031, 3, 21), apiDay(2031, 3, 25))    // starts the day after `to`

	status, body, _ := apiGet(t, base, "/api/v1/bookings?from=2031-03-10&to=2031-03-20", apiTestKey)
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	got := decodeBookings(t, body)
	var purposes []string
	for _, b := range got.Data {
		purposes = append(purposes, b.Purpose)
	}
	want := []string{"straddles-from", "inside", "last-day"}
	if len(purposes) != len(want) {
		t.Fatalf("got %v, want %v", purposes, want)
	}
	for i := range want { // ordered by start time
		if purposes[i] != want[i] {
			t.Fatalf("got %v, want %v", purposes, want)
		}
	}
	if got.Meta.Total != 3 || got.Meta.Limit != apiDefaultLimit || got.Meta.Offset != 0 {
		t.Fatalf("meta: %+v", got.Meta)
	}
	// `to` as a date includes that whole day: meta.to is the next midnight.
	if !got.Meta.To.Equal(apiDay(2031, 3, 21)) || !got.Meta.From.Equal(apiDay(2031, 3, 10)) {
		t.Fatalf("meta range: %v .. %v", got.Meta.From, got.Meta.To)
	}
}

func TestAPIBookings_RFC3339Bounds(t *testing.T) {
	base, st := apiTestServer(t, "dev-01")
	mustBook(t, st, "dev-01", "ana", "p", apiDay(2031, 4, 10), apiDay(2031, 4, 11))

	// Window entirely after the booking ends -> empty (end is exclusive-overlap).
	from := url.QueryEscape(apiDay(2031, 4, 11).Format(time.RFC3339))
	to := url.QueryEscape(apiDay(2031, 4, 12).Format(time.RFC3339))
	_, body, _ := apiGet(t, base, "/api/v1/bookings?from="+from+"&to="+to, apiTestKey)
	if got := decodeBookings(t, body); got.Meta.Total != 0 || got.Data == nil {
		t.Fatalf("want empty non-null data, got %+v (%s)", got, body)
	}

	// Window overlapping by an hour -> found.
	from = url.QueryEscape(apiDay(2031, 4, 10).Add(-time.Hour).Format(time.RFC3339))
	to = url.QueryEscape(apiDay(2031, 4, 10).Add(time.Hour).Format(time.RFC3339))
	_, body, _ = apiGet(t, base, "/api/v1/bookings?from="+from+"&to="+to, apiTestKey)
	if got := decodeBookings(t, body); got.Meta.Total != 1 {
		t.Fatalf("want 1, got %+v", got.Meta)
	}
}

func TestAPIBookings_PlusOffsetNotEncoded(t *testing.T) {
	// A client that forgets to percent-encode "+07:00" sends a space instead.
	base, st := apiTestServer(t, "dev-01")
	mustBook(t, st, "dev-01", "ana", "p", apiDay(2031, 5, 10), apiDay(2031, 5, 11))
	status, body, _ := apiGet(t, base, "/api/v1/bookings?from=2031-05-10T00:00:00+07:00&to=2031-05-12T00:00:00+07:00", apiTestKey)
	if status != 200 || decodeBookings(t, body).Meta.Total != 1 {
		t.Fatalf("got %d %s", status, body)
	}
}

func TestAPIBookings_FiltersAndFields(t *testing.T) {
	base, st := apiTestServer(t, "dev-01", "dev-02")
	a := mustBook(t, st, "dev-01", "ana", "feature x", apiDay(2031, 6, 1), apiDay(2031, 6, 10))
	mustBook(t, st, "dev-02", "bob", "hotfix", apiDay(2031, 6, 1), apiDay(2031, 6, 10))
	if err := st.Release(a.ID, store.ResReleased); err != nil {
		t.Fatal(err)
	}

	r := "from=2031-06-01&to=2031-06-30"
	_, body, _ := apiGet(t, base, "/api/v1/bookings?"+r+"&slot=dev-02", apiTestKey)
	if got := decodeBookings(t, body); got.Meta.Total != 1 || got.Data[0].Slot != "dev-02" {
		t.Fatalf("slot filter: %s", body)
	}
	_, body, _ = apiGet(t, base, "/api/v1/bookings?"+r+"&status=released", apiTestKey)
	got := decodeBookings(t, body)
	if got.Meta.Total != 1 {
		t.Fatalf("status filter: %s", body)
	}
	b := got.Data[0]
	if b.Username != "ana" || b.Purpose != "feature x" || b.Status != store.ResReleased {
		t.Fatalf("fields: %+v", b)
	}
	if b.ReleasedAt == nil || b.CreatedAt.IsZero() || !b.Start.Equal(apiDay(2031, 6, 1)) {
		t.Fatalf("times: %+v", b)
	}
	// Released far before its planned end -> effectiveEnd is the release time.
	if !b.EffectiveEnd.Equal(*b.ReleasedAt) || !b.EffectiveEnd.Before(b.End) {
		t.Fatalf("effectiveEnd %v, releasedAt %v, end %v", b.EffectiveEnd, b.ReleasedAt, b.End)
	}
	// The internal user id must not leak.
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	first := raw["data"].([]any)[0].(map[string]any)
	for _, banned := range []string{"UserID", "userId", "user_id"} {
		if _, ok := first[banned]; ok {
			t.Fatalf("field %q leaked", banned)
		}
	}
}

func TestAPIBookings_Pagination(t *testing.T) {
	base, st := apiTestServer(t, "dev-01")
	for i := 0; i < 5; i++ { // five back-to-back bookings, distinct days
		mustBook(t, st, "dev-01", "ana", "p", apiDay(2031, 7, 1+i), apiDay(2031, 7, 2+i))
	}
	page := func(q string) apiBookingsResponse {
		_, body, _ := apiGet(t, base, "/api/v1/bookings?from=2031-07-01&to=2031-07-31"+q, apiTestKey)
		return decodeBookings(t, body)
	}
	p1 := page("&limit=2&offset=0")
	p2 := page("&limit=2&offset=2")
	p3 := page("&limit=2&offset=4")
	p4 := page("&limit=2&offset=99")
	if len(p1.Data) != 2 || len(p2.Data) != 2 || len(p3.Data) != 1 || len(p4.Data) != 0 {
		t.Fatalf("page sizes: %d %d %d %d", len(p1.Data), len(p2.Data), len(p3.Data), len(p4.Data))
	}
	for _, p := range []apiBookingsResponse{p1, p2, p3, p4} {
		if p.Meta.Total != 5 {
			t.Fatalf("total: %+v", p.Meta)
		}
	}
	seen := map[int64]bool{}
	var last time.Time
	for _, p := range []apiBookingsResponse{p1, p2, p3} {
		for _, b := range p.Data {
			if seen[b.ID] {
				t.Fatalf("booking %d repeated across pages", b.ID)
			}
			seen[b.ID] = true
			if b.Start.Before(last) {
				t.Fatal("pages must be ordered by start")
			}
			last = b.Start
		}
	}
	if len(seen) != 5 {
		t.Fatalf("saw %d distinct, want 5", len(seen))
	}
}
