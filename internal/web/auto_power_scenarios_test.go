package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"parkee/staging-platform/internal/store"
)

// quiet waits long enough for a (wrongly) started background stop/start to
// show up in the docker log, then reports how many compose calls ran.
func quiet(logFile string) int {
	time.Sleep(250 * time.Millisecond)
	return len(composeCalls(logFile))
}

func bookWindow(t *testing.T, st *store.Store, slot string, from, to time.Time) {
	t.Helper()
	u, err := st.UserByUsername("boss")
	if err != nil || u == nil {
		t.Fatalf("user boss: %v", err)
	}
	if _, err := st.CreateReservation(slot, u.ID, "boss", "scenario", from, to); err != nil {
		t.Fatal(err)
	}
}

// 1. A stop countdown is running and someone books.
func TestScenario1_BookingDuringStopCountdown(t *testing.T) {
	bin, logFile := fakeDockerBin(t)
	srv, ts, st := newTestServerFull(t, func(c *testConfig) { c.dockerBin = bin }, "dev-04")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	post(t, admin, ts.URL+"/admin/envs/auto", url.Values{"on": {"1"}}).Body.Close()

	// The countdown is on screen.
	if !strings.Contains(body(t, admin, ts.URL+"/"), `class="countdown"`) {
		t.Fatal("expected a stop countdown on the dashboard")
	}

	// Someone books for today: the countdown disappears and nothing is stopped
	// even when the stop time has passed.
	bookWindow(t, st, "dev-04", time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
	if strings.Contains(body(t, admin, ts.URL+"/"), `class="countdown"`) {
		t.Fatal("countdown must disappear once the env is booked")
	}
	srv.runAutoPower(time.Now().Add(16 * time.Minute))
	if n := quiet(logFile); n != 0 {
		t.Fatalf("booked env was stopped (%d compose calls)", n)
	}
}

// 1b. The stop time is 15 min away and a booking begins soon after: cancelled.
// A booking far away does NOT cancel the stop.
func TestScenario1_BookingStartingSoonCancelsFarOneDoesNot(t *testing.T) {
	bin, logFile := fakeDockerBin(t)
	srv, ts, st := newTestServerFull(t, func(c *testConfig) { c.dockerBin = bin }, "dev-04")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	post(t, admin, ts.URL+"/admin/envs/auto", url.Values{"on": {"1"}}).Body.Close()
	t1 := time.Now()

	// Booking begins 40 min from now: inside the 30 min quiet window that
	// follows the stop moment (15 min from now), so no stop.
	bookWindow(t, st, "dev-04", t1.Add(40*time.Minute), t1.Add(2*time.Hour))
	srv.runAutoPower(t1.Add(16 * time.Minute))
	if n := quiet(logFile); n != 0 {
		t.Fatalf("booking in 40 min must cancel the stop, got %d compose calls", n)
	}
}

func TestScenario1_FarBookingStillStops(t *testing.T) {
	bin, logFile := fakeDockerBin(t)
	srv, ts, st := newTestServerFull(t, func(c *testConfig) { c.dockerBin = bin }, "dev-04")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	post(t, admin, ts.URL+"/admin/envs/auto", url.Values{"on": {"1"}}).Body.Close()
	t1 := time.Now()

	// Booking tomorrow: far outside the window, so the env is still stopped now
	// and started again 15 min before the booking.
	bookWindow(t, st, "dev-04", t1.Add(24*time.Hour), t1.Add(30*time.Hour))
	srv.runAutoPower(t1.Add(16 * time.Minute))
	waitFor(t, "stop", func() bool { return len(composeCalls(logFile)) == 1 })
	if composeCalls(logFile)[0] != "compose -p p04 stop" {
		t.Fatalf("got %v", composeCalls(logFile))
	}
}

// The plan is made at the start of a tick; a booking can appear before the
// stop is carried out. The stop must be dropped.
func TestScenario1_StopIsRecheckedAgainstBookingRightBeforeActing(t *testing.T) {
	bin, logFile := fakeDockerBin(t)
	srv, ts, st := newTestServerFull(t, func(c *testConfig) { c.dockerBin = bin }, "dev-04")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	post(t, admin, ts.URL+"/admin/envs/auto", url.Values{"on": {"1"}}).Body.Close()

	// Booked right now (real time) until 10 min from now. Planning for a moment
	// 16 min from now sees that booking as over, so it plans a stop; only the
	// check made just before acting (real time) can catch it.
	bookWindow(t, st, "dev-04", time.Now().Add(-time.Hour), time.Now().Add(10*time.Minute))
	srv.runAutoPower(time.Now().Add(16 * time.Minute))
	if n := quiet(logFile); n != 0 {
		t.Fatalf("an env booked right now was stopped (%d compose calls)", n)
	}
}

// 2. The env is stopped and a booking is running: it is started on the next tick.
func TestScenario2_StoppedEnvWithRunningBookingStartsAtOnce(t *testing.T) {
	bin, logFile := fakeDockerBin(t)
	srv, ts, st := newTestServerFull(t, func(c *testConfig) { c.dockerBin = bin }, "dev-04")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	post(t, admin, ts.URL+"/admin/envs/auto", url.Values{"on": {"1"}}).Body.Close()
	if err := st.SetDesiredState("dev-04", store.DesiredStopped); err != nil {
		t.Fatal(err)
	}

	bookWindow(t, st, "dev-04", time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
	srv.runAutoPower(time.Now())
	waitFor(t, "start", func() bool { return len(composeCalls(logFile)) == 1 })
	if got := composeCalls(logFile)[0]; got != "compose -p p04 start" {
		t.Fatalf("got %q", got)
	}
	waitFor(t, "desired running", func() bool {
		e, _ := st.GetEnvironment("dev-04")
		return e.DesiredState == store.DesiredRunning
	})
}

// 3. A start countdown is running (booking tomorrow) and someone books today:
// the start is brought forward to right now. An earlier booking that begins
// later today moves the countdown to 15 min before it.
func TestScenario3_NewerEarlierBookingBringsStartForward(t *testing.T) {
	bin, logFile := fakeDockerBin(t)
	srv, ts, st := newTestServerFull(t, func(c *testConfig) { c.dockerBin = bin }, "dev-04")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	post(t, admin, ts.URL+"/admin/envs/auto", url.Values{"on": {"1"}}).Body.Close()
	if err := st.SetDesiredState("dev-04", store.DesiredStopped); err != nil {
		t.Fatal(err)
	}
	t1 := time.Now()

	// Start countdown for a booking in 24 h: nothing starts yet.
	bookWindow(t, st, "dev-04", t1.Add(24*time.Hour), t1.Add(30*time.Hour))
	srv.runAutoPower(t1)
	if n := quiet(logFile); n != 0 {
		t.Fatalf("start is 24 h away, but compose ran %d time(s)", n)
	}
	if !strings.Contains(body(t, admin, ts.URL+"/"), "auto start") {
		t.Fatal("dashboard should show the start countdown")
	}

	// A booking that begins in 2 h: the start moves to 15 min before it.
	bookWindow(t, st, "dev-04", t1.Add(2*time.Hour), t1.Add(4*time.Hour))
	srv.runAutoPower(t1.Add(time.Hour))
	if n := quiet(logFile); n != 0 {
		t.Fatalf("start is due 1 h 45 min from now, but compose ran %d time(s)", n)
	}
	srv.runAutoPower(t1.Add(105*time.Minute + time.Second))
	waitFor(t, "start 15 min before the new booking", func() bool { return len(composeCalls(logFile)) == 1 })
	if composeCalls(logFile)[0] != "compose -p p04 start" {
		t.Fatalf("got %v", composeCalls(logFile))
	}
}

func TestScenario3_BookingForTodayStartsStoppedEnvAtOnce(t *testing.T) {
	bin, logFile := fakeDockerBin(t)
	srv, ts, st := newTestServerFull(t, func(c *testConfig) { c.dockerBin = bin }, "dev-04")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	post(t, admin, ts.URL+"/admin/envs/auto", url.Values{"on": {"1"}}).Body.Close()
	if err := st.SetDesiredState("dev-04", store.DesiredStopped); err != nil {
		t.Fatal(err)
	}
	t1 := time.Now()
	bookWindow(t, st, "dev-04", t1.Add(24*time.Hour), t1.Add(30*time.Hour)) // countdown running
	srv.runAutoPower(t1)
	if n := quiet(logFile); n != 0 {
		t.Fatalf("not due yet, %d compose calls", n)
	}

	// Someone books today (a whole-day booking has already started).
	bookWindow(t, st, "dev-04", t1.Add(-time.Minute), t1.Add(time.Hour))
	srv.runAutoPower(time.Now())
	waitFor(t, "start now", func() bool { return len(composeCalls(logFile)) == 1 })
}

// A booking that begins in under 15 min: the start is already due.
func TestScenario3_BookingBeginningInTenMinutesStartsNow(t *testing.T) {
	bin, logFile := fakeDockerBin(t)
	srv, ts, st := newTestServerFull(t, func(c *testConfig) { c.dockerBin = bin }, "dev-04")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	post(t, admin, ts.URL+"/admin/envs/auto", url.Values{"on": {"1"}}).Body.Close()
	if err := st.SetDesiredState("dev-04", store.DesiredStopped); err != nil {
		t.Fatal(err)
	}
	bookWindow(t, st, "dev-04", time.Now().Add(10*time.Minute), time.Now().Add(3*time.Hour))
	srv.runAutoPower(time.Now())
	waitFor(t, "start", func() bool { return len(composeCalls(logFile)) == 1 })
}
