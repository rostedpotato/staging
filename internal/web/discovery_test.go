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

// fakeDocker writes a script that prints canned `docker ps` output.
func fakeDocker(t *testing.T, lines string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(p, []byte("#!/bin/sh\ncat <<'EOF'\n"+lines+"\nEOF\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func waitRun(t *testing.T, st *store.Store, want string) *store.DiscoveryRun {
	t.Helper()
	for i := 0; i < 100; i++ {
		runs, _ := st.RecentDiscoveryRuns(1)
		if len(runs) == 1 && runs[0].Status == want {
			return &runs[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("discovery run never reached %q", want)
	return nil
}

func TestDiscovery_RBAC(t *testing.T) {
	ts, st := newTestServer(t)
	eng := provisionAndLogin(t, ts, st, "dev", store.RoleEngineering)
	r := post(t, eng, ts.URL+"/admin/discovery/run", url.Values{"servers": {"staging"}})
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("engineer run discovery: want 403, got %d", r.StatusCode)
	}
}

func TestDiscovery_ReviewThenApply(t *testing.T) {
	docker := fakeDocker(t, "watersheep-ho|0.0.0.0:9000->9005/tcp\nfisherman-ho|0.0.0.0:4000->8888/tcp\n"+
		"watersheep-dev-new|0.0.0.0:9030->9005/tcp\nkafka-ho|0.0.0.0:10000->9092/tcp")
	ts, st := newTestServerWith(t, func(c *testConfig) { c.dockerBin = docker }, "ho", "dev-old")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)

	r := post(t, admin, ts.URL+"/admin/discovery/run", url.Values{"servers": {"staging"}})
	r.Body.Close()
	run := waitRun(t, st, store.RunReady)

	// Nothing is written before the admin saves.
	if envs, _ := st.ListEnvironments(store.ServerStaging); len(envs) != 2 {
		t.Fatalf("inventory changed before review: %+v", envs)
	}
	page := body(t, admin, ts.URL+"/admin/discovery")
	for _, want := range []string{"dev-new", "dev-old", "missing", "Save"} {
		if !strings.Contains(page, want) {
			t.Fatalf("review page missing %q", want)
		}
	}

	// A second run is refused while this one waits for review.
	r = post(t, admin, ts.URL+"/admin/discovery/run", url.Values{"servers": {"staging"}})
	r.Body.Close()
	if loc := r.Header.Get("Location"); !strings.Contains(loc, "already+running") {
		t.Fatalf("second run should be refused, redirect=%q", loc)
	}

	r = post(t, admin, ts.URL+"/admin/discovery/decide", url.Values{"id": {itoa(run.ID)}, "action": {"apply"}})
	r.Body.Close()

	got := map[string]store.Environment{}
	envs, _ := st.ListEnvironments(store.ServerStaging)
	for _, e := range envs {
		got[e.Name] = e
	}
	if got["ho"].Ports["watersheep"] != 9000 || got["ho"].Ports["fisherman"] != 4000 {
		t.Fatalf("ho ports not saved: %+v", got["ho"])
	}
	if got["dev-new"].Status != store.EnvActive {
		t.Fatalf("dev-new should be active: %+v", got["dev-new"])
	}
	if got["dev-old"].Status != store.EnvMissing {
		t.Fatalf("dev-old should be missing: %+v", got["dev-old"])
	}

	// Missing envs drop out of the booking list; new ones appear.
	bookings := body(t, admin, ts.URL+"/bookings")
	if strings.Contains(bookings, `value="dev-old"`) || !strings.Contains(bookings, `value="dev-new"`) {
		t.Fatal("booking list not updated after apply")
	}

	// The run can't be applied twice.
	r = post(t, admin, ts.URL+"/admin/discovery/decide", url.Values{"id": {itoa(run.ID)}, "action": {"apply"}})
	r.Body.Close()
	if loc := r.Header.Get("Location"); !strings.Contains(loc, "already+decided") {
		t.Fatalf("second apply should be refused, redirect=%q", loc)
	}
}

func TestDiscovery_DiscardChangesNothing(t *testing.T) {
	docker := fakeDocker(t, "watersheep-brand-new|0.0.0.0:9099->9005/tcp")
	ts, st := newTestServerWith(t, func(c *testConfig) { c.dockerBin = docker }, "ho")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)

	r := post(t, admin, ts.URL+"/admin/discovery/run", url.Values{"servers": {"staging"}})
	r.Body.Close()
	run := waitRun(t, st, store.RunReady)
	r = post(t, admin, ts.URL+"/admin/discovery/decide", url.Values{"id": {itoa(run.ID)}, "action": {"discard"}})
	r.Body.Close()

	envs, _ := st.ListEnvironments("")
	if len(envs) != 1 || envs[0].Name != "ho" || envs[0].Status != store.EnvActive {
		t.Fatalf("discard must not change inventory: %+v", envs)
	}
}

func TestDiscovery_AutomationUnreachableKeepsEnvs(t *testing.T) {
	ts, st := newTestServerWith(t, func(c *testConfig) {
		c.dockerBin = fakeDocker(t, "watersheep-ho|0.0.0.0:9000->9005/tcp")
		c.sshBin = "/bin/false" // ssh fails
	}, "ho")
	if err := st.ApplyEnvChanges([]store.EnvChange{{Name: "0vt", Server: store.ServerAutomation, Kind: "new"}}); err != nil {
		t.Fatal(err)
	}
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)

	r := post(t, admin, ts.URL+"/admin/discovery/run", url.Values{"servers": {"all"}})
	r.Body.Close()
	run := waitRun(t, st, store.RunReady)
	if run.Result.Errors[store.ServerAutomation] == "" {
		t.Fatal("automation error should be reported")
	}
	for _, c := range run.Result.Changes {
		if c.Name == "0vt" {
			t.Fatalf("0vt must not appear in the diff when SSH failed, got %q", c.Kind)
		}
	}
}
