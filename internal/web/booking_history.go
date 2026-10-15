package web

import (
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"time"

	"parkee/staging-platform/internal/store"
)

// The dashboard shows bookings from historyDaysBack days ago to
// historyDaysFwd days ahead.
const (
	historyDaysBack = 7
	historyDaysFwd  = 7
)

// Display statuses of a history row, next to the stored ones
// (store.ResReleased / store.ResOverridden).
const (
	histActive   = "active"   // running today
	histUpcoming = "upcoming" // starts later
	histExpired  = "expired"  // ran its full course
)

// historyRow is one line of the dashboard booking table. Bookings of the same
// person, env and purpose on consecutive days are merged into one row.
type historyRow struct {
	Slot         string
	Username     string
	Purpose      string
	Status       string
	Dates        string // booked dates, e.g. "2026-10-15 → 2026-10-22 (8 days)"
	CreatedNote  string // when it was booked
	ReleasedText string // release time, only for released/overridden bookings
	Start        time.Time
}

func midnight(t time.Time) time.Time {
	t = t.In(bookingLoc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, bookingLoc)
}

// fmtStamp formats a moment as local "2006-01-02 15:04" ("" for zero).
func fmtStamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(bookingLoc).Format("2006-01-02 15:04")
}

// inclusiveDays counts the calendar days from start to end (end exclusive by
// one second, so 23:59:59 belongs to its own day).
func inclusiveDays(start, end time.Time) int {
	last := midnight(end.Add(-time.Second))
	n := int(math.Round(last.Sub(midnight(start)).Hours()/24)) + 1
	if n < 1 {
		return 1
	}
	return n
}

// fmtRange shows the booked dates, without the meaningless 00:00 / 23:59
// of whole-day bookings: "2026-10-15" or "2026-10-15 → 2026-10-22 (8 days)".
func fmtRange(start, end time.Time) string {
	if start.IsZero() {
		return "—"
	}
	a := start.In(bookingLoc).Format("2006-01-02")
	b := end.Add(-time.Second).In(bookingLoc).Format("2006-01-02")
	if a == b {
		return a
	}
	return fmt.Sprintf("%s → %s (%d days)", a, b, inclusiveDays(start, end))
}

// mergeable bookings have no release time of their own, so a run of them can
// be shown as one row.
func mergeable(r store.Reservation) bool {
	return r.Status == store.ResActive || r.Status == store.ResExpired
}

// groupHistory merges back-to-back bookings of the same person, env and
// purpose into one row each, then sorts the rows by sortKey.
func groupHistory(rs []store.Reservation, now time.Time, sortKey string) []historyRow {
	items := append([]store.Reservation(nil), rs...)
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		switch {
		case a.Slot != b.Slot:
			return a.Slot < b.Slot
		case a.Username != b.Username:
			return a.Username < b.Username
		case a.Purpose != b.Purpose:
			return a.Purpose < b.Purpose
		}
		return a.Start.Before(b.Start)
	})
	var runs [][]store.Reservation
	for _, r := range items {
		if n := len(runs); n > 0 && mergeable(r) {
			last := runs[n-1][len(runs[n-1])-1]
			if mergeable(last) && last.Slot == r.Slot && last.Username == r.Username &&
				last.Purpose == r.Purpose && !r.Start.After(last.End.Add(time.Minute)) {
				runs[n-1] = append(runs[n-1], r)
				continue
			}
		}
		runs = append(runs, []store.Reservation{r})
	}
	rows := make([]historyRow, 0, len(runs))
	for _, run := range runs {
		rows = append(rows, historyRowFor(run, now))
	}
	sortHistory(rows, sortKey)
	return rows
}

func historyRowFor(run []store.Reservation, now time.Time) historyRow {
	first := run[0]
	row := historyRow{Slot: first.Slot, Username: first.Username, Purpose: first.Purpose, Start: first.Start}
	end := first.End
	created := first.CreatedTime()
	running, anyActive := false, false
	for _, r := range run {
		if r.End.After(end) {
			end = r.End
		}
		if c := r.CreatedTime(); !c.IsZero() && (created.IsZero() || c.Before(created)) {
			created = c
		}
		if r.Status == store.ResActive && r.End.After(now) {
			anyActive = true
			if !r.Start.After(now) {
				running = true
			}
		}
	}
	switch {
	case first.Status == store.ResReleased || first.Status == store.ResOverridden:
		row.Status = first.Status
		row.ReleasedText = fmtStamp(first.ReleasedAt)
		if !first.ReleasedAt.After(first.Start) {
			row.ReleasedText += " (before it started)"
		}
	case running:
		row.Status = histActive
	case anyActive:
		row.Status = histUpcoming
	default:
		row.Status = histExpired
	}
	row.Dates = fmtRange(first.Start, end)
	if len(run) == 1 {
		row.CreatedNote = "booked " + fmtStamp(created)
	} else {
		row.CreatedNote = fmt.Sprintf("%d bookings, first made %s", len(run), fmtStamp(created))
	}
	return row
}

func sortHistory(rows []historyRow, key string) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch key {
		case "date_asc":
			if !a.Start.Equal(b.Start) {
				return a.Start.Before(b.Start)
			}
			return a.Slot < b.Slot
		case "slot_asc":
			if a.Slot != b.Slot {
				return a.Slot < b.Slot
			}
			return a.Start.After(b.Start)
		case "slot_desc":
			if a.Slot != b.Slot {
				return a.Slot > b.Slot
			}
			return a.Start.After(b.Start)
		}
		if !a.Start.Equal(b.Start) {
			return a.Start.After(b.Start)
		}
		return a.Slot < b.Slot
	})
}

// filterHistory keeps the rows of one status group. The default ("" or
// anything unknown) is what matters now: running and upcoming bookings.
func filterHistory(rows []historyRow, status string) []historyRow {
	var out []historyRow
	for _, r := range rows {
		keep := false
		switch status {
		case "all":
			keep = true
		case "past":
			keep = r.Status == histExpired
		case "released":
			keep = r.Status == store.ResReleased || r.Status == store.ResOverridden
		default:
			keep = r.Status == histActive || r.Status == histUpcoming
		}
		if keep {
			out = append(out, r)
		}
	}
	return out
}

// --- calendar matrix --------------------------------------------------------

type matrixDay struct {
	Weekday string
	Label   string
	Today   bool
	Weekend bool
}

// matrixCell is one cell of an env row: a free day, or a booking bar that
// spans Span days.
type matrixCell struct {
	Span  int
	Class string
	User  string
	Tip   string
	Hue   int // colour of the person, 0 for free cells
}

type matrixRow struct {
	Env   string
	Cells []matrixCell
}

type matrixData struct {
	Days []matrixDay
	Rows []matrixRow
}

// userHue gives every person a stable colour, away from red so a booking never
// looks like a "down" status.
func userHue(name string) int {
	h := fnv.New32a()
	h.Write([]byte(name))
	return 40 + int(h.Sum32()%280)
}

// buildMatrix lays the bookings out with one row per env and one column per
// day. A day counts as booked for as long as the booking really held the env
// (see EffectiveEnd), so an early release frees the days after it. Days that
// are over are marked "past". Consecutive days of the same person and purpose
// become one bar.
func buildMatrix(envs []string, rs []store.Reservation, now time.Time, meID int64) matrixData {
	first := midnight(now).AddDate(0, 0, -historyDaysBack)
	n := historyDaysBack + historyDaysFwd + 1
	m := matrixData{Days: make([]matrixDay, n)}
	for i := range m.Days {
		d := first.AddDate(0, 0, i)
		m.Days[i] = matrixDay{
			Weekday: d.Format("Mon"),
			Label:   d.Format("2 Jan"),
			Today:   i == historyDaysBack,
			Weekend: d.Weekday() == time.Saturday || d.Weekday() == time.Sunday,
		}
	}
	bySlot := map[string][]*store.Reservation{}
	for i := range rs {
		bySlot[rs[i].Slot] = append(bySlot[rs[i].Slot], &rs[i])
	}
	for _, env := range envs {
		owner := make([]*store.Reservation, n)
		live := make([]bool, n)
		for _, r := range bySlot[env] {
			end := r.EffectiveEnd()
			if !end.After(r.Start) {
				continue // released before it started: held nothing
			}
			isLive := r.Status == store.ResActive && r.End.After(now)
			for i := 0; i < n; i++ {
				ds := first.AddDate(0, 0, i)
				if !r.Start.Before(ds.AddDate(0, 0, 1)) || !end.After(ds) {
					continue
				}
				// A live booking beats a finished one; otherwise the later one wins.
				if owner[i] == nil || (isLive && !live[i]) || (isLive == live[i] && r.Start.After(owner[i].Start)) {
					owner[i], live[i] = r, isLive
				}
			}
		}
		row := matrixRow{Env: env}
		for i := 0; i < n; {
			o := owner[i]
			if o == nil {
				cls := "free"
				if m.Days[i].Weekend {
					cls += " wk"
				}
				if m.Days[i].Today {
					cls += " today"
				}
				row.Cells = append(row.Cells, matrixCell{Span: 1, Class: cls})
				i++
				continue
			}
			past := !live[i] || i < historyDaysBack
			j := i + 1
			for j < n && owner[j] != nil && owner[j].Username == o.Username && owner[j].Purpose == o.Purpose &&
				(!live[j] || j < historyDaysBack) == past {
				j++
			}
			row.Cells = append(row.Cells, segmentCell(owner[i:j], past, meID, now))
			i = j
		}
		m.Rows = append(m.Rows, row)
	}
	return m
}

// segmentCell builds one booking bar from the bookings covering its days.
func segmentCell(members []*store.Reservation, past bool, meID int64, now time.Time) matrixCell {
	o := members[0]
	start, end := o.Start, o.End
	same := true
	for _, r := range members {
		if r.Start.Before(start) {
			start = r.Start
		}
		if r.End.After(end) {
			end = r.End
		}
		if r.ID != o.ID {
			same = false
		}
	}
	cls := "booked"
	if past {
		cls += " past"
	}
	if meID != 0 && o.UserID == meID {
		cls += " mine"
	}
	who := o.Username
	if o.Purpose != "" {
		who += " — " + o.Purpose
	}
	lines := []string{who, fmtRange(start, end)}
	switch {
	case o.Status == store.ResReleased || o.Status == store.ResOverridden:
		lines = append(lines, o.Status+" "+fmtStamp(o.ReleasedAt))
	case o.Status == store.ResExpired || !o.End.After(now):
		lines = append(lines, "finished")
	case o.Start.After(now):
		lines = append(lines, "upcoming")
	default:
		lines = append(lines, "active")
	}
	if same {
		lines = append(lines, "booked "+fmtStamp(o.CreatedTime()))
	}
	return matrixCell{Span: len(members), Class: cls, User: o.Username, Tip: strings.Join(lines, "\n"), Hue: userHue(o.Username)}
}
