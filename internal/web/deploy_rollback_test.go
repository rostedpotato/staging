package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"parkee/staging-platform/internal/auth"
	"parkee/staging-platform/internal/config"
	"parkee/staging-platform/internal/deploy"
	"parkee/staging-platform/internal/discovery"
	"parkee/staging-platform/internal/store"
)

// fakeProvider is an in-memory Jenkins stand-in that always succeeds fast.
type fakeProvider struct{}

func (fakeProvider) Trigger(context.Context, string, map[string]string) (string, error) {
	return "queue://1", nil
}
func (fakeProvider) ResolveBuild(context.Context, string) (int64, string, error) {
	return 1, "build://1", nil
}
func (fakeProvider) BuildStatus(context.Context, string, int64) (bool, string, error) {
	return false, "SUCCESS", nil
}
func (fakeProvider) ConsoleText(context.Context, string, int64) (string, error) {
	return "ok\n", nil
}

// newDeployTestServer builds the stack with deploy enabled + a fake provider.
func newDeployTestServer(t *testing.T, slots ...string) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/w.db")
	if err != nil {
		t.Fatal(err)
	}
	appRoot := t.TempDir()
	for _, name := range slots {
		dir := appRoot + "/agent-" + name + "/parkee-agent-backoffice"
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(dir+"/.env", []byte("VITE_GIT_BRANCH=b\n"), 0o644)
	}
	cfg := &config.Config{}
	cfg.Auth.SessionTTLHours = 1
	cfg.Discovery.AppRoot = appRoot
	cfg.Discovery.SlotDirPrefix = "agent-"
	cfg.Discovery.RepoSubdir = "parkee-agent-backoffice"
	cfg.Discovery.NginxSitesDir = t.TempDir()
	cfg.Discovery.DockerBin = "true"
	cfg.Deploy.Enabled = true
	cfg.Deploy.PollSeconds = 1
	cfg.Deploy.RequireBooking = false
	cfg.Deploy.Services = []config.DeployService{
		{Key: "ws", Label: "WS", Job: "deploy-ws", TagParam: true},
	}

	a := auth.New(cfg, st)
	disc := discovery.New(cfg.Discovery)
	dep := deploy.New(cfg, st, fakeProvider{})
	srv, err := NewServer(cfg, disc, a, st, dep)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func waitDep(t *testing.T, st *store.Store, id int64, want string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		d, _ := st.DeploymentByID(id)
		if d != nil && d.Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("deployment %d did not reach %q", id, want)
}

func TestRollback_AdminOnly(t *testing.T) {
	ts, st := newDeployTestServer(t, "ho")
	_ = provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	eng := provisionAndLogin(t, ts, st, "dev", store.RoleEngineering)

	// Non-admin cannot rollback.
	resp := post(t, eng, ts.URL+"/deploy/rollback", url.Values{"from": {"1"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("engineer rollback: want 403, got %d", resp.StatusCode)
	}
}

func TestRollback_Flow(t *testing.T) {
	ts, st := newDeployTestServer(t, "ho")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)

	// Deploy v1, then v2 (both succeed via fake provider).
	post(t, admin, ts.URL+"/deploy/start",
		url.Values{"service": {"ws"}, "slot": {"ho"}, "branch": {"v1"}}).Body.Close()
	waitDep(t, st, 1, store.DepSuccess)
	post(t, admin, ts.URL+"/deploy/start",
		url.Values{"service": {"ws"}, "slot": {"ho"}, "branch": {"v2"}}).Body.Close()
	waitDep(t, st, 2, store.DepSuccess)

	// Rollback from deployment #2 -> should create #3 targeting v1.
	resp := post(t, admin, ts.URL+"/deploy/rollback", url.Values{"from": {"2"}})
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if !strings.Contains(loc, "/deployments/3") {
		t.Fatalf("rollback should redirect to new deployment, got %q", loc)
	}
	waitDep(t, st, 3, store.DepSuccess)

	d3, _ := st.DeploymentByID(3)
	if d3.Ref != "v1" {
		t.Fatalf("rollback target ref: want v1, got %q", d3.Ref)
	}
}

func TestRollback_NoTarget(t *testing.T) {
	ts, st := newDeployTestServer(t, "ho")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)

	// Only one successful deploy exists -> nothing to roll back to.
	post(t, admin, ts.URL+"/deploy/start",
		url.Values{"service": {"ws"}, "slot": {"ho"}, "branch": {"only"}}).Body.Close()
	waitDep(t, st, 1, store.DepSuccess)

	resp := post(t, admin, ts.URL+"/deploy/rollback", url.Values{"from": {"1"}})
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if !strings.Contains(loc, "error=") {
		t.Fatalf("expected error redirect for no rollback target, got %q", loc)
	}
}
