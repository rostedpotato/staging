package web

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"parkee/staging-platform/internal/auth"
	"parkee/staging-platform/internal/config"
	"parkee/staging-platform/internal/deploy"
	"parkee/staging-platform/internal/discovery"
	"parkee/staging-platform/internal/jenkins"
	"parkee/staging-platform/internal/store"
)

// newTestServer builds the full HTTP stack with a temp DB. Optional slot
// names are seeded as discoverable slot dirs under AppRoot.
func newTestServer(t *testing.T, slots ...string) (*httptest.Server, *store.Store) {
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
		if err := os.WriteFile(dir+"/.env", []byte("VITE_GIT_BRANCH=b\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{}
	cfg.Auth.SessionTTLHours = 1
	cfg.Discovery.AppRoot = appRoot
	cfg.Discovery.SlotDirPrefix = "agent-"
	cfg.Discovery.RepoSubdir = "parkee-agent-backoffice"
	cfg.Discovery.NginxSitesDir = t.TempDir()
	cfg.Discovery.DockerBin = "true"

	a := auth.New(cfg, st)
	disc := discovery.New(cfg.Discovery)
	dep := deploy.New(cfg, st, jenkins.New("", "", ""))
	srv, err := NewServer(cfg, disc, a, st, dep)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

const testFinalPassword = "FinalPassw0rd!"

// provisionUser creates an account directly in the store, simulating what an
// admin would do via /admin/users. Returns the user and the temp password.
func provisionUser(t *testing.T, st *store.Store, username, role string) (*store.User, string) {
	t.Helper()
	temp, err := store.GenerateTempPassword()
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateUser(username, username, role, temp)
	if err != nil {
		t.Fatal(err)
	}
	return u, temp
}

// login authenticates with username/password and returns a client with the
// session cookie. If the account requires a password reset (fresh accounts
// always do), it completes the reset with testFinalPassword so the returned
// client is fully usable against protected pages.
func login(t *testing.T, ts *httptest.Server, username, password string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := c.PostForm(ts.URL+"/auth/login", url.Values{"username": {username}, "password": {password}})
	if err != nil {
		t.Fatal(err)
	}
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if strings.Contains(loc, "/reset-password") {
		resp2 := post(t, c, ts.URL+"/reset-password",
			url.Values{"new_password": {testFinalPassword}, "confirm_password": {testFinalPassword}})
		resp2.Body.Close()
	}
	return c
}

// provisionAndLogin creates an account with the given role and returns a
// ready-to-use logged-in client (password already reset).
func provisionAndLogin(t *testing.T, ts *httptest.Server, st *store.Store, username, role string) *http.Client {
	t.Helper()
	_, temp := provisionUser(t, st, username, role)
	return login(t, ts, username, temp)
}

func post(t *testing.T, c *http.Client, u string, form url.Values) *http.Response {
	t.Helper()
	resp, err := c.PostForm(u, form)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAdminUsers_RBAC(t *testing.T) {
	ts, st := newTestServer(t)
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	eng := provisionAndLogin(t, ts, st, "dev", store.RoleEngineering)

	// Admin can view the users page.
	r, _ := admin.Get(ts.URL + "/admin/users")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("admin /admin/users: want 200, got %d", r.StatusCode)
	}
	r.Body.Close()

	// Engineering is forbidden.
	r, _ = eng.Get(ts.URL + "/admin/users")
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("eng /admin/users: want 403, got %d", r.StatusCode)
	}
	r.Body.Close()
}

func TestAdminUsers_PromoteDemote(t *testing.T) {
	ts, st := newTestServer(t)
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	dev, _ := provisionUser(t, st, "dev", store.RoleEngineering)

	if dev.Role != store.RoleEngineering {
		t.Fatalf("dev should start engineering, got %s", dev.Role)
	}

	// Promote dev -> admin.
	resp := post(t, admin, ts.URL+"/admin/users/role",
		url.Values{"id": {itoa(dev.ID)}, "role": {"admin"}})
	resp.Body.Close()
	got, _ := st.UserByUsername("dev")
	if got.Role != store.RoleAdmin {
		t.Fatalf("promote failed, role=%s", got.Role)
	}

	// Demote dev back -> engineering (boss is still admin).
	resp = post(t, admin, ts.URL+"/admin/users/role",
		url.Values{"id": {itoa(dev.ID)}, "role": {"engineering"}})
	resp.Body.Close()
	got, _ = st.UserByUsername("dev")
	if got.Role != store.RoleEngineering {
		t.Fatalf("demote failed, role=%s", got.Role)
	}
}

func TestAdminUsers_CannotDemoteLastAdmin(t *testing.T) {
	ts, st := newTestServer(t)
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin) // sole admin

	boss, _ := st.UserByUsername("boss")
	resp := post(t, admin, ts.URL+"/admin/users/role",
		url.Values{"id": {itoa(boss.ID)}, "role": {"engineering"}})
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if !strings.Contains(loc, "error=") {
		t.Fatalf("expected error redirect, got %q", loc)
	}
	boss, _ = st.UserByUsername("boss")
	if boss.Role != store.RoleAdmin {
		t.Fatalf("last admin was demoted, role=%s", boss.Role)
	}
}

func TestAdminUsers_CannotDisableSelf(t *testing.T) {
	ts, st := newTestServer(t)
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)

	boss, _ := st.UserByUsername("boss")
	resp := post(t, admin, ts.URL+"/admin/users/status",
		url.Values{"id": {itoa(boss.ID)}, "status": {"disabled"}})
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if !strings.Contains(loc, "error=") {
		t.Fatalf("expected error redirect, got %q", loc)
	}
	boss, _ = st.UserByUsername("boss")
	if boss.Status != "active" {
		t.Fatalf("admin disabled self, status=%s", boss.Status)
	}
}

func TestAdminUsers_DisableEngineerBlocksLogin(t *testing.T) {
	ts, st := newTestServer(t)
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	dev, tempPass := provisionUser(t, st, "dev", store.RoleEngineering)

	resp := post(t, admin, ts.URL+"/admin/users/status",
		url.Values{"id": {itoa(dev.ID)}, "status": {"disabled"}})
	resp.Body.Close()

	got, _ := st.UserByUsername("dev")
	if got.Status != "disabled" {
		t.Fatalf("disable failed, status=%s", got.Status)
	}

	// A disabled account must not be able to log in.
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp2 := post(t, c, ts.URL+"/auth/login", url.Values{"username": {"dev"}, "password": {tempPass}})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disabled user login: want 401, got %d", resp2.StatusCode)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
