package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"parkee/staging-platform/internal/store"
)

func day(offset int) string { return time.Now().AddDate(0, 0, offset).Format("2006-01-02") }

func TestParseBookingRange(t *testing.T) {
	today := time.Now().In(bookingLoc)

	// Single day (no end date): 00:00:00 .. 23:59:59 of that day.
	s, e, err := parseBookingRange(day(1), "")
	if err != nil {
		t.Fatal(err)
	}
	if e.Sub(s) != 24*time.Hour-time.Second {
		t.Fatalf("single day span = %v", e.Sub(s))
	}

	// 14 days, both ends included: ends at 23:59:59 of the 14th day.
	s, e, err = parseBookingRange(day(1), day(14))
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Sub(s); got != 14*24*time.Hour-time.Second {
		t.Fatalf("14-day span = %v", got)
	}
	if e.Hour() != 23 || e.Minute() != 59 || e.Second() != 59 {
		t.Fatalf("end should be 23:59:59, got %v", e)
	}

	// Starting today is allowed (only past days are rejected).
	if _, _, err := parseBookingRange(today.Format("2006-01-02"), ""); err != nil {
		t.Fatalf("today should be bookable: %v", err)
	}

	for name, c := range map[string][2]string{
		"missing start":  {"", day(3)},
		"bad start":      {"nope", ""},
		"bad end":        {day(1), "31-12-2026"},
		"end before":     {day(5), day(2)},
		"past start":     {day(-1), day(3)},
		"over the limit": {day(1), day(1 + maxBookingDays)}, // maxBookingDays+1 days
	} {
		if _, _, err := parseBookingRange(c[0], c[1]); err == nil {
			t.Fatalf("%s: want error", name)
		}
	}
	// Exactly the limit is fine.
	if _, _, err := parseBookingRange(day(1), day(maxBookingDays)); err != nil {
		t.Fatalf("exactly %d days should be allowed: %v", maxBookingDays, err)
	}
}

func TestBookingPage_GroupsStagingAndAutomation(t *testing.T) {
	ts, st := newTestServer(t, "ho", "dev-09")
	if err := st.ApplyEnvChanges([]store.EnvChange{{Name: "0vt", Server: store.ServerAutomation, Kind: "new"}}); err != nil {
		t.Fatal(err)
	}
	u := provisionAndLogin(t, ts, st, "qa", store.RoleEngineering)
	page := body(t, u, ts.URL+"/bookings")

	sIdx := strings.Index(page, `<optgroup label="Staging">`)
	aIdx := strings.Index(page, `<optgroup label="Staging Automation">`)
	if sIdx < 0 || aIdx < 0 || sIdx > aIdx {
		t.Fatalf("want Staging group before Staging Automation group (staging=%d automation=%d)", sIdx, aIdx)
	}
	stagingGroup, autoGroup := page[sIdx:aIdx], page[aIdx:]
	if !strings.Contains(stagingGroup, `value="ho"`) || !strings.Contains(stagingGroup, `value="dev-09"`) {
		t.Fatal("staging envs missing from the Staging group")
	}
	if strings.Contains(stagingGroup, `value="0vt"`) {
		t.Fatal("automation env leaked into the Staging group")
	}
	if !strings.Contains(autoGroup, `value="0vt"`) || strings.Contains(autoGroup, `value="ho"`) {
		t.Fatal("0vt must be only in the Staging Automation group")
	}
	for _, want := range []string{`id="bookCal"`, `type="hidden" name="startDate"`, `type="hidden" name="endDate"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("booking form missing %s", want)
		}
	}
}

func TestBookingCreate_DateRange(t *testing.T) {
	ts, st := newTestServer(t, "ho")
	u := provisionAndLogin(t, ts, st, "qa", store.RoleEngineering)
	other := provisionAndLogin(t, ts, st, "dev", store.RoleEngineering)

	// 14-day booking.
	r := post(t, u, ts.URL+"/bookings/create", url.Values{
		"slot": {"ho"}, "startDate": {day(1)}, "endDate": {day(14)}, "purpose": {"regression"}})
	r.Body.Close()
	if loc := r.Header.Get("Location"); !strings.Contains(loc, "notice=") {
		t.Fatalf("14-day booking should succeed, redirect=%q", loc)
	}
	active, _ := st.ListActive()
	if len(active) != 1 || active[0].End.Sub(active[0].Start) != 14*24*time.Hour-time.Second {
		t.Fatalf("stored booking wrong: %+v", active)
	}

	// Any day inside that range is taken for someone else; the day after is free.
	r = post(t, other, ts.URL+"/bookings/create", url.Values{"slot": {"ho"}, "startDate": {day(7)}})
	r.Body.Close()
	if loc := r.Header.Get("Location"); !strings.Contains(loc, "already+booked+by+qa") {
		t.Fatalf("overlap inside the range must be refused, redirect=%q", loc)
	}
	r = post(t, other, ts.URL+"/bookings/create", url.Values{"slot": {"ho"}, "startDate": {day(15)}})
	r.Body.Close()
	if loc := r.Header.Get("Location"); !strings.Contains(loc, "notice=") {
		t.Fatalf("the day after the range should be free, redirect=%q", loc)
	}

	// Over the limit is refused with a clear message.
	r = post(t, u, ts.URL+"/bookings/create", url.Values{
		"slot": {"ho"}, "startDate": {day(40)}, "endDate": {day(40 + maxBookingDays)}})
	r.Body.Close()
	if loc := r.Header.Get("Location"); !strings.Contains(loc, "at+most+30+days") {
		t.Fatalf("too-long booking must be refused, redirect=%q", loc)
	}
}

func TestBookingPage_SendsBookedDaysPerSlot(t *testing.T) {
	ts, st := newTestServer(t, "ho", "dev-09")
	owner := provisionAndLogin(t, ts, st, "budi", store.RoleEngineering)
	viewer := provisionAndLogin(t, ts, st, "qa", store.RoleEngineering)

	// A hostile purpose must not be able to break out of the <script> block.
	evil := `</script><script>alert("x")</script>`
	r := post(t, owner, ts.URL+"/bookings/create", url.Values{
		"slot": {"ho"}, "startDate": {day(2)}, "endDate": {day(4)}, "purpose": {evil}})
	r.Body.Close()
	r = post(t, owner, ts.URL+"/bookings/create", url.Values{"slot": {"dev-09"}, "startDate": {day(6)}})
	r.Body.Close()

	page := body(t, viewer, ts.URL+"/bookings")
	i := strings.Index(page, "var booked = ")
	if i < 0 {
		t.Fatal("page has no booked-days data")
	}
	data := page[i : i+strings.Index(page[i:], ";\n")]

	for _, want := range []string{`"ho":[`, `"dev-09":[`, `"user":"budi"`, `"s":"` + day(2) + `"`, `"e":"` + day(4) + `"`, `"mine":false`} {
		if !strings.Contains(data, want) {
			t.Fatalf("booked data missing %s:\n%s", want, data)
		}
	}
	if strings.Contains(page, evil) || strings.Contains(page, `<script>alert("x")`) {
		t.Fatal("purpose leaked into the page unescaped")
	}

	// The owner sees their own booking flagged as theirs.
	if own := body(t, owner, ts.URL+"/bookings"); !strings.Contains(own, `"mine":true`) {
		t.Fatal(`owner's booking should have "mine":true`)
	}
	// An env nobody booked has no entry.
	if strings.Contains(data, `"0vt":`) {
		t.Fatal("unbooked env must not appear in the data")
	}
}
