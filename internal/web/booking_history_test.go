package web

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"parkee/staging-platform/internal/store"
)

// A fixed "now" in the middle of the day, in the booking timezone.
var hNow = time.Date(2026, 10, 15, 14, 0, 0, 0, bookingLoc)

// dayRes is a whole-day booking from date d1 to d2 (day offsets from hNow).
func dayRes(id int64, slot, user, purpose, status string, d1, d2 int) store.Reservation {
	start := midnight(hNow).AddDate(0, 0, d1)
	end := midnight(hNow).AddDate(0, 0, d2+1).Add(-time.Second)
	return store.Reservation{
		ID: id, UserID: 1, Slot: slot, Username: user, Purpose: purpose, Status: status,
		Start: start, End: end, CreatedAt: start.AddDate(0, 0, -3).UTC().Format(time.RFC3339),
	}
}

func TestEffectiveEnd(t *testing.T) {
	r := dayRes(1, "a", "u", "p", store.ResExpired, -1, 0)
	// The platform noticed the end much later: that is NOT the end.
	r.ReleasedAt = r.End.Add(10 * time.Hour)
	if !r.EffectiveEnd().Equal(r.End) {
		t.Fatalf("expired booking must end at its planned end, got %v", r.EffectiveEnd())
	}
	// Released early: the release time is the end.
	r2 := dayRes(2, "a", "u", "p", store.ResReleased, 0, 5)
	r2.ReleasedAt = hNow
	if !r2.EffectiveEnd().Equal(hNow) {
		t.Fatalf("released booking must end at the release time, got %v", r2.EffectiveEnd())
	}
}

func TestGroupHistory_MergesConsecutiveDays(t *testing.T) {
	var rs []store.Reservation
	for i := 0; i <= 7; i++ { // brian, dev-04, today .. +7, one booking per day
		rs = append(rs, dayRes(int64(i+1), "development-04", "brian", "dev multiple zone gp", store.ResActive, i, i))
	}
	rs = append(rs, dayRes(20, "development-10", "samsul", "hotfix", store.ResActive, 0, 1))

	rows := groupHistory(rs, hNow, "date_desc")
	if len(rows) != 2 {
		t.Fatalf("8 daily bookings of one person must be one row; got %d rows: %+v", len(rows), rows)
	}
	var brian historyRow
	for _, r := range rows {
		if r.Username == "brian" {
			brian = r
		}
	}
	if brian.Dates != "2026-10-15 → 2026-10-22 (8 days)" {
		t.Fatalf("dates: %q", brian.Dates)
	}
	if strings.Contains(brian.Dates, "00:00") || strings.Contains(brian.Dates, "23:59") {
		t.Fatal("whole-day bookings must not show 00:00 / 23:59")
	}
	if brian.Status != histActive {
		t.Fatalf("status: %q", brian.Status)
	}
	if !strings.HasPrefix(brian.CreatedNote, "8 bookings") {
		t.Fatalf("created note: %q", brian.CreatedNote)
	}
}

func TestGroupHistory_DoesNotMergeDifferentPeopleGapsOrPurposes(t *testing.T) {
	rs := []store.Reservation{
		dayRes(1, "a", "brian", "p", store.ResActive, 0, 0),
		dayRes(2, "a", "samsul", "p", store.ResActive, 1, 1),    // other person
		dayRes(3, "a", "brian", "p", store.ResActive, 3, 3),     // gap of 2 days
		dayRes(4, "a", "brian", "other", store.ResActive, 4, 4), // other purpose
		dayRes(5, "b", "brian", "p", store.ResActive, 1, 1),     // other env
	}
	if got := len(groupHistory(rs, hNow, "date_desc")); got != 5 {
		t.Fatalf("nothing here may merge, got %d rows", got)
	}
}

func TestGroupHistory_ReleasedStaysOwnRow(t *testing.T) {
	rel := dayRes(1, "06r", "qa", "smoke", store.ResReleased, 4, 13)
	rel.ReleasedAt = hNow.Add(-3 * time.Hour) // released before it started
	next := dayRes(2, "06r", "qa", "smoke", store.ResActive, 4, 13)
	rows := groupHistory([]store.Reservation{rel, next}, hNow, "date_desc")
	if len(rows) != 2 {
		t.Fatalf("a released booking must keep its own row, got %d", len(rows))
	}
	var released historyRow
	for _, r := range rows {
		if r.Status == store.ResReleased {
			released = r
		}
	}
	if !strings.Contains(released.ReleasedText, fmtStamp(rel.ReleasedAt)) ||
		!strings.Contains(released.ReleasedText, "before it started") {
		t.Fatalf("released text: %q", released.ReleasedText)
	}
}

func TestGroupHistory_StatusAndFilter(t *testing.T) {
	rs := []store.Reservation{
		dayRes(1, "a", "u1", "p", store.ResActive, -1, 1),   // running now
		dayRes(2, "b", "u2", "p", store.ResActive, 2, 3),    // upcoming
		dayRes(3, "c", "u3", "p", store.ResExpired, -5, -4), // finished
		dayRes(4, "d", "u4", "p", store.ResReleased, 0, 3),  // released
	}
	rs[3].ReleasedAt = hNow
	rows := groupHistory(rs, hNow, "slot_asc")
	status := map[string]string{}
	for _, r := range rows {
		status[r.Slot] = r.Status
	}
	want := map[string]string{"a": histActive, "b": histUpcoming, "c": histExpired, "d": store.ResReleased}
	for k, v := range want {
		if status[k] != v {
			t.Fatalf("env %s: status %q, want %q", k, status[k], v)
		}
	}
	count := func(show string) int { return len(filterHistory(rows, show)) }
	if count("") != 2 || count("active") != 2 {
		t.Fatalf("default view = running + upcoming (2), got %d", count(""))
	}
	if count("past") != 1 || count("released") != 1 || count("all") != 4 {
		t.Fatalf("filters: past=%d released=%d all=%d", count("past"), count("released"), count("all"))
	}
}

func TestBuildMatrix(t *testing.T) {
	rs := []store.Reservation{
		dayRes(1, "dev-04", "brian", "p", store.ResActive, 0, 3), // today .. +3
		dayRes(2, "dev-10", "samsul", "p", store.ResExpired, -3, -2),
		dayRes(3, "06r", "qa", "p", store.ResReleased, 0, 5), // released today at 14:00
	}
	rs[2].ReleasedAt = hNow
	m := buildMatrix([]string{"dev-04", "dev-10", "06r", "free-env"}, rs, hNow, 1)

	if len(m.Days) != 15 || !m.Days[historyDaysBack].Today {
		t.Fatalf("want 15 days with today at index %d, got %d", historyDaysBack, len(m.Days))
	}
	for _, row := range m.Rows {
		total := 0
		for _, c := range row.Cells {
			total += c.Span
		}
		if total != 15 {
			t.Fatalf("%s: cells span %d days, want 15", row.Env, total)
		}
	}
	find := func(env string) matrixRow {
		for _, r := range m.Rows {
			if r.Env == env {
				return r
			}
		}
		t.Fatalf("no row for %s", env)
		return matrixRow{}
	}
	// dev-04: one 4-day bar for brian, marked as mine (UserID 1).
	var bar matrixCell
	for _, c := range find("dev-04").Cells {
		if c.User == "brian" {
			bar = c
		}
	}
	if bar.Span != 4 || !strings.Contains(bar.Class, "mine") || strings.Contains(bar.Class, "past") {
		t.Fatalf("dev-04 bar: %+v", bar)
	}
	if !strings.Contains(bar.Tip, "2026-10-15 → 2026-10-18") || !strings.Contains(bar.Tip, "active") {
		t.Fatalf("tooltip: %q", bar.Tip)
	}
	// dev-10: finished booking is greyed.
	var old matrixCell
	for _, c := range find("dev-10").Cells {
		if c.User == "samsul" {
			old = c
		}
	}
	if old.Span != 2 || !strings.Contains(old.Class, "past") {
		t.Fatalf("dev-10 finished bar: %+v", old)
	}
	// 06r: released today, so only today (and the past) is filled; the days after are free.
	for _, c := range find("06r").Cells {
		if c.User == "qa" && c.Span > 1 {
			t.Fatalf("a booking released today must not fill the days after it: %+v", c)
		}
	}
	// free env: only free cells.
	for _, c := range find("free-env").Cells {
		if c.User != "" {
			t.Fatalf("free env has a booking cell: %+v", c)
		}
	}
}

func TestBuildMatrix_ReleasedBeforeStartHoldsNothing(t *testing.T) {
	r := dayRes(1, "06r", "qa", "p", store.ResReleased, 4, 13)
	r.ReleasedAt = hNow.Add(-3 * time.Hour)
	m := buildMatrix([]string{"06r"}, []store.Reservation{r}, hNow, 0)
	for _, c := range m.Rows[0].Cells {
		if c.User != "" {
			t.Fatalf("booking released before it started must not appear: %+v", c)
		}
	}
}

func TestDashboard_ShowsMatrixAndMergedTable(t *testing.T) {
	ts, st := newTestServer(t, "ho", "dev-09")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	u, _ := st.UserByUsername("boss")
	for i := 1; i <= 3; i++ { // 3 consecutive single days
		start := midnight(time.Now()).AddDate(0, 0, i)
		if _, err := st.CreateReservation("ho", u.ID, "boss", "regression", start, start.AddDate(0, 0, 1).Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	page := body(t, admin, ts.URL+"/")
	if !strings.Contains(page, `class="matrix"`) {
		t.Fatal("dashboard should show the booking calendar")
	}
	// "(3 days)" also appears in the calendar bar's tooltip, so count table cells only.
	if strings.Count(page, "(3 days)</td>") != 1 {
		t.Fatalf("the 3 consecutive bookings should be one table row (3 days), page:\n%s", page)
	}
	// Other filters render and don't error.
	for _, q := range []string{"?hshow=past", "?hshow=released", "?hshow=all", "?hslot=ho&hsort=slot_asc", "?hshow=bogus"} {
		if !strings.Contains(body(t, admin, ts.URL+"/"+q), "<h2>Bookings</h2>") {
			t.Fatalf("dashboard %s did not render", q)
		}
	}
	// Release one: its row moves to the "released" filter with its release time.
	list, _ := st.ListActive()
	post(t, admin, ts.URL+"/bookings/release", url.Values{"id": {strconv.FormatInt(list[0].ID, 10)}}).Body.Close()
	rel := body(t, admin, ts.URL+"/?hshow=released")
	if !strings.Contains(rel, "tag hist-released") {
		t.Fatal("released booking should be listed under Released early")
	}
}
