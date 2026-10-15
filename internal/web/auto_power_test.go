package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"parkee/staging-platform/internal/store"
)

var t0 = time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)

func res(user string, start, end time.Time) *store.Reservation {
	return &store.Reservation{Username: user, Start: start, End: end}
}

func TestPlanFor(t *testing.T) {
	cases := []struct {
		name   string
		in     planInput
		action string
		at     time.Time
	}{
		{
			name:   "running, booked now: nothing",
			in:     planInput{Desired: store.DesiredRunning, Current: res("a", t0.Add(-time.Hour), t0.Add(time.Hour))},
			action: "",
		},
		{
			name:   "running, booking ended 5 min ago: stop 15 min after the end",
			in:     planInput{Desired: store.DesiredRunning, LastEnd: t0.Add(-5 * time.Minute), AutoSince: t0.Add(-48 * time.Hour)},
			action: "stop", at: t0.Add(10 * time.Minute),
		},
		{
			name:   "running, booking ended long ago, auto just switched on: 15 min from switch-on",
			in:     planInput{Desired: store.DesiredRunning, LastEnd: t0.Add(-48 * time.Hour), AutoSince: t0},
			action: "stop", at: t0.Add(15 * time.Minute),
		},
		{
			name:   "running, started manually 2 min ago: 15 min from the start",
			in:     planInput{Desired: store.DesiredRunning, AutoSince: t0.Add(-48 * time.Hour), DesiredSince: t0.Add(-2 * time.Minute)},
			action: "stop", at: t0.Add(13 * time.Minute),
		},
		{
			name: "running, next booking soon: no stop (would restart at once)",
			in: planInput{Desired: store.DesiredRunning, LastEnd: t0.Add(-5 * time.Minute), AutoSince: t0.Add(-48 * time.Hour),
				Next: res("b", t0.Add(20*time.Minute), t0.Add(5*time.Hour))},
			action: "",
		},
		{
			name: "running, next booking tomorrow: stop",
			in: planInput{Desired: store.DesiredRunning, LastEnd: t0.Add(-5 * time.Minute), AutoSince: t0.Add(-48 * time.Hour),
				Next: res("b", t0.Add(24*time.Hour), t0.Add(30*time.Hour))},
			action: "stop", at: t0.Add(10 * time.Minute),
		},
		{
			name:   "stopped, booking tomorrow: start 15 min before",
			in:     planInput{Desired: store.DesiredStopped, Next: res("b", t0.Add(24*time.Hour), t0.Add(30*time.Hour))},
			action: "start", at: t0.Add(24*time.Hour - 15*time.Minute),
		},
		{
			name:   "stopped, booking already running: start now",
			in:     planInput{Desired: store.DesiredStopped, Current: res("a", t0.Add(-time.Hour), t0.Add(time.Hour))},
			action: "start", at: t0,
		},
		{
			name:   "stopped, no booking: nothing",
			in:     planInput{Desired: store.DesiredStopped},
			action: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := planFor(c.in, t0)
			if p.Action != c.action {
				t.Fatalf("action: want %q, got %q (%+v)", c.action, p.Action, p)
			}
			if c.action != "" && !p.At.Equal(c.at) {
				t.Fatalf("at: want %s, got %s", c.at, p.At)
			}
		})
	}
}

func TestAutoPower_Toggle_RBACAndAudit(t *testing.T) {
	ts, st := newTestServer(t, "dev-04")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	eng := provisionAndLogin(t, ts, st, "dev", store.RoleEngineering)

	r := post(t, eng, ts.URL+"/admin/envs/auto", url.Values{"on": {"1"}})
	r.Body.Close()
	if r.StatusCode != 403 {
		t.Fatalf("engineer toggle: want 403, got %d", r.StatusCode)
	}
	if on, _ := st.AutoPower(); on {
		t.Fatal("engineer must not switch auto power on")
	}

	post(t, admin, ts.URL+"/admin/envs/auto", url.Values{"on": {"1"}}).Body.Close()
	if on, _ := st.AutoPower(); !on {
		t.Fatal("admin should switch auto power on")
	}
	page := body(t, admin, ts.URL+"/admin/envs")
	if !strings.Contains(page, "Turn automation off") || !strings.Contains(page, "auto stop") {
		t.Fatal("page should show the ON state and a countdown")
	}
	if !strings.Contains(body(t, admin, ts.URL+"/"), `class="countdown"`) {
		t.Fatal("dashboard should show the countdown")
	}

	post(t, admin, ts.URL+"/admin/envs/auto", url.Values{"on": {"0"}}).Body.Close()
	if on, _ := st.AutoPower(); on {
		t.Fatal("admin should switch auto power off")
	}
	if strings.Contains(body(t, admin, ts.URL+"/"), `class="countdown"`) {
		t.Fatal("no countdown while auto power is off")
	}
	if !strings.Contains(body(t, admin, ts.URL+"/audit"), "SET_AUTO_POWER") {
		t.Fatal("toggle must be audited")
	}
}

// The scheduler stops an idle env once its time has come, and starts it
// again before a booking - using the same compose path as the buttons.
func TestAutoPower_StopsIdleThenStartsBeforeBooking(t *testing.T) {
	bin, logFile := fakeDockerBin(t)
	srv, ts, st := newTestServerFull(t, func(c *testConfig) { c.dockerBin = bin }, "dev-04")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	post(t, admin, ts.URL+"/admin/envs/auto", url.Values{"on": {"1"}}).Body.Close()

	// Not yet due: nothing happens.
	srv.runAutoPower(time.Now())
	time.Sleep(200 * time.Millisecond)
	if n := len(composeCalls(logFile)); n != 0 {
		t.Fatalf("nothing is due yet, but %d compose call(s) ran", n)
	}

	// 16 minutes later the idle env is stopped.
	srv.runAutoPower(time.Now().Add(16 * time.Minute))
	waitFor(t, "auto stop", func() bool { return len(composeCalls(logFile)) == 1 })
	if got := composeCalls(logFile)[0]; got != "compose -p p04 stop" {
		t.Fatalf("auto stop ran %q", got)
	}
	waitFor(t, "desired stopped", func() bool {
		e, _ := st.GetEnvironment("dev-04")
		return e.DesiredState == store.DesiredStopped
	})

	// A booking tomorrow: start is planned 15 min before, not now.
	u, _ := st.UserByUsername("boss")
	start := time.Now().Add(24 * time.Hour)
	if _, err := st.CreateReservation("dev-04", u.ID, "boss", "t", start, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stop finished", func() bool { return !srv.isBusy("dev-04") })
	srv.runAutoPower(time.Now())
	time.Sleep(200 * time.Millisecond)
	if n := len(composeCalls(logFile)); n != 1 {
		t.Fatalf("start is not due yet, but compose ran %d time(s)", n)
	}
	srv.runAutoPower(start.Add(-14 * time.Minute))
	waitFor(t, "auto start", func() bool { return len(composeCalls(logFile)) == 2 })
	if got := composeCalls(logFile)[1]; got != "compose -p p04 start" {
		t.Fatalf("auto start ran %q", got)
	}
	if !strings.Contains(body(t, admin, ts.URL+"/audit"), "auto-power") {
		t.Fatal("automatic actions must be audited as auto-power")
	}
}

func TestAutoPower_OffDoesNothing(t *testing.T) {
	bin, logFile := fakeDockerBin(t)
	srv, _, _ := newTestServerFull(t, func(c *testConfig) { c.dockerBin = bin }, "dev-04")
	srv.runAutoPower(time.Now().Add(48 * time.Hour))
	time.Sleep(200 * time.Millisecond)
	if n := len(composeCalls(logFile)); n != 0 {
		t.Fatalf("auto power is off, but compose ran %d time(s)", n)
	}
}

func TestAutoPower_DoesNotStopWhileDeploying(t *testing.T) {
	bin, logFile := fakeDockerBin(t)
	srv, _, st := newTestServerFull(t, func(c *testConfig) { c.dockerBin = bin }, "dev-04")
	_ = st.SetAutoPower(true)
	u, _ := provisionUser(t, st, "boss", store.RoleAdmin)
	if _, err := st.CreateDeployment("dev-04", "ws", "branch", "main", u.ID, "boss", "deploy-ws"); err != nil {
		t.Fatal(err)
	}
	srv.runAutoPower(time.Now().Add(time.Hour))
	time.Sleep(200 * time.Millisecond)
	if n := len(composeCalls(logFile)); n != 0 {
		t.Fatalf("env with a running deployment was stopped (%d compose calls)", n)
	}
}
