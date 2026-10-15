package web

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"parkee/staging-platform/internal/store"
)

// fakeDockerBin logs every call to a file and answers `inspect` like docker
// does for the two compose containers of env dev-04.
func fakeDockerBin(t *testing.T) (bin, logFile string) {
	t.Helper()
	dir := t.TempDir()
	logFile = filepath.Join(dir, "calls.log")
	bin = filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logFile + "\n" +
		"if [ \"$1\" = inspect ]; then\n" +
		"  printf '/watersheep-dev-04|p04\\n/fisherman-dev-04|p04\\n'\n" +
		"fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logFile
}

func composeCalls(logFile string) []string {
	b, _ := os.ReadFile(logFile)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(l, "compose") {
			out = append(out, l)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func locationOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	return resp.Header.Get("Location")
}

func TestEnvPower_RBAC(t *testing.T) {
	ts, st := newTestServer(t, "dev-04")
	eng := provisionAndLogin(t, ts, st, "dev", store.RoleEngineering)
	for _, p := range []string{"/admin/envs", "/admin/envs/stop", "/admin/envs/start"} {
		var resp *http.Response
		if p == "/admin/envs" {
			resp, _ = eng.Get(ts.URL + p)
		} else {
			resp = post(t, eng, ts.URL+p, url.Values{"env": {"dev-04"}})
		}
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("engineer %s: want 403, got %d", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if e, _ := st.GetEnvironment("dev-04"); e.DesiredState != store.DesiredRunning {
		t.Fatal("engineer must not be able to stop an env")
	}
}

func TestEnvPower_StopThenStart(t *testing.T) {
	bin, logFile := fakeDockerBin(t)
	ts, st := newTestServerWith(t, func(c *testConfig) { c.dockerBin = bin }, "dev-04")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)

	loc := locationOf(t, post(t, admin, ts.URL+"/admin/envs/stop", url.Values{"env": {"dev-04"}}))
	if !strings.Contains(loc, "notice=") {
		t.Fatalf("stop should succeed, redirected to %s", loc)
	}
	waitFor(t, "compose stop call", func() bool { return len(composeCalls(logFile)) == 1 })
	got := composeCalls(logFile)
	if got[0] != "compose -p p04 stop" {
		t.Fatalf("wrong stop calls: %v", got)
	}
	if e, _ := st.GetEnvironment("dev-04"); e.DesiredState != store.DesiredStopped {
		t.Fatal("desired state should be stopped")
	}

	// The dashboard shows the env as stopped (grey), not down (red).
	waitFor(t, "stopped badge", func() bool {
		return strings.Contains(body(t, admin, ts.URL+"/"), `badge stopped`)
	})

	// Starting it again.
	loc = locationOf(t, post(t, admin, ts.URL+"/admin/envs/start", url.Values{"env": {"dev-04"}}))
	if !strings.Contains(loc, "notice=") {
		t.Fatalf("start should succeed, redirected to %s", loc)
	}
	waitFor(t, "compose start call", func() bool { return len(composeCalls(logFile)) == 2 })
	got = composeCalls(logFile)
	if got[1] != "compose -p p04 start" {
		t.Fatalf("wrong start calls: %v", got)
	}
	waitFor(t, "desired state running", func() bool {
		e, _ := st.GetEnvironment("dev-04")
		return e.DesiredState == store.DesiredRunning
	})

	// Every step is audited.
	audit := body(t, admin, ts.URL+"/audit")
	for _, a := range []string{"STOP_ENV", "START_ENV"} {
		if !strings.Contains(audit, a) {
			t.Fatalf("audit log missing %s", a)
		}
	}
}

func TestEnvPower_RefusesBookedEnv(t *testing.T) {
	bin, logFile := fakeDockerBin(t)
	ts, st := newTestServerWith(t, func(c *testConfig) { c.dockerBin = bin }, "dev-04")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	u, _ := st.UserByUsername("boss")
	if _, err := st.CreateReservation("dev-04", u.ID, "boss", "testing", time.Now().Add(-time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	loc := locationOf(t, post(t, admin, ts.URL+"/admin/envs/stop", url.Values{"env": {"dev-04"}}))
	if !strings.Contains(loc, "error=") {
		t.Fatalf("booked env must be refused, redirected to %s", loc)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(composeCalls(logFile)); n != 0 {
		t.Fatalf("no compose command may run for a booked env, saw %d", n)
	}
	if e, _ := st.GetEnvironment("dev-04"); e.DesiredState != store.DesiredRunning {
		t.Fatal("desired state must stay running")
	}
}

func TestEnvPower_UnknownEnv(t *testing.T) {
	ts, st := newTestServer(t, "dev-04")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	loc := locationOf(t, post(t, admin, ts.URL+"/admin/envs/stop", url.Values{"env": {"nope"}}))
	if !strings.Contains(loc, "error=") {
		t.Fatalf("unknown env must be refused, got %s", loc)
	}
}

func TestEnvPower_FailureIsReportedAndAudited(t *testing.T) {
	// docker that fails on `compose`.
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = inspect ]; then printf '/watersheep-dev-04|p04\\n/fisherman-dev-04|p04\\n'; exit 0; fi\n" +
		"echo boom >&2; exit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ts, st := newTestServerWith(t, func(c *testConfig) { c.dockerBin = bin }, "dev-04")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)

	post(t, admin, ts.URL+"/admin/envs/stop", url.Values{"env": {"dev-04"}}).Body.Close()
	waitFor(t, "failure audit", func() bool {
		return strings.Contains(body(t, admin, ts.URL+"/audit"), "STOP_ENV_FAILED")
	})
	if !strings.Contains(body(t, admin, ts.URL+"/admin/envs"), "last action failed") {
		t.Fatal("failure should be visible on /admin/envs")
	}
}

func TestDeploy_RefusedWhileEnvStopped_WBOAllowed(t *testing.T) {
	ts, st := newDeployTestServer(t, "dev-04")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	if err := st.SetDesiredState("dev-04", store.DesiredStopped); err != nil {
		t.Fatal(err)
	}

	loc := locationOf(t, post(t, admin, ts.URL+"/deploy/start",
		url.Values{"slot": {"dev-04"}, "service": {"ws"}, "branch": {"main"}}))
	if !strings.Contains(loc, "error=") || !strings.Contains(loc, "stopped") {
		t.Fatalf("ws deploy on a stopped env must be refused, got %s", loc)
	}
	loc = locationOf(t, post(t, admin, ts.URL+"/deploy/start",
		url.Values{"slot": {"dev-04"}, "service": {"wbo"}, "branch": {"main"}}))
	if strings.Contains(loc, "error=") {
		t.Fatalf("wbo deploy is static and must stay allowed, got %s", loc)
	}
}
