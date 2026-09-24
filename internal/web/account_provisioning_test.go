package web

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"parkee/staging-platform/internal/store"
)

// TestLogin_UnknownUsernameDenied verifies logging in with a username that
// has no account is rejected with generic invalid-credentials messaging.
func TestLogin_UnknownUsernameDenied(t *testing.T) {
	ts, st := newTestServer(t)
	_ = provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp := post(t, c, ts.URL+"/auth/login", url.Values{"username": {"stranger"}, "password": {"whatever"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown username: want 401, got %d", resp.StatusCode)
	}
	if n, _ := st.CountUsers(); n != 1 {
		t.Fatalf("login must not create an account; want 1 user, got %d", n)
	}
}

// TestAdminCreateUser_ThenLoginWorks verifies the intended flow: admin creates
// the account (getting a temp password), and only then can that person log
// in — and they are forced through the reset-password flow first.
func TestAdminCreateUser_ThenLoginWorks(t *testing.T) {
	ts, st := newTestServer(t)
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)

	resp := post(t, admin, ts.URL+"/admin/users/create",
		url.Values{"username": {"newdev"}, "name": {"New Dev"}, "role": {"engineering"}})
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if !strings.Contains(loc, "notice=") || !strings.Contains(loc, "newpass=") {
		t.Fatalf("expected success notice with temp password, got %q", loc)
	}
	tempPass := extractQueryParam(t, loc, "newpass")

	u, _ := st.UserByUsername("newdev")
	if u == nil || u.Role != store.RoleEngineering {
		t.Fatalf("account was not created correctly: %+v", u)
	}
	if !u.MustResetPassword {
		t.Fatal("new account must require a password reset")
	}

	// Logging in with the temp password must redirect to reset-password.
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp2 := post(t, c, ts.URL+"/auth/login", url.Values{"username": {"newdev"}, "password": {tempPass}})
	loc2 := resp2.Header.Get("Location")
	resp2.Body.Close()
	if !strings.Contains(loc2, "/reset-password") {
		t.Fatalf("first login should redirect to /reset-password, got %q", loc2)
	}

	// Before resetting, protected pages also redirect to reset-password (not the dashboard).
	r, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusSeeOther || !strings.Contains(r.Header.Get("Location"), "/reset-password") {
		t.Fatalf("want 303 to /reset-password, got %d Location=%q", r.StatusCode, r.Header.Get("Location"))
	}

	// Complete the reset; now the dashboard is reachable.
	resp3 := post(t, c, ts.URL+"/reset-password",
		url.Values{"new_password": {"BrandNewPass1!"}, "confirm_password": {"BrandNewPass1!"}})
	resp3.Body.Close()
	r2, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("after reset: want 200 dashboard, got %d", r2.StatusCode)
	}
}

// TestAdminCreateUser_DuplicateRejected verifies creating the same username twice fails.
func TestAdminCreateUser_DuplicateRejected(t *testing.T) {
	ts, st := newTestServer(t)
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)

	post(t, admin, ts.URL+"/admin/users/create",
		url.Values{"username": {"dup"}, "role": {"engineering"}}).Body.Close()

	resp := post(t, admin, ts.URL+"/admin/users/create",
		url.Values{"username": {"dup"}, "role": {"engineering"}})
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if !strings.Contains(loc, "error=") {
		t.Fatalf("expected error redirect for duplicate username, got %q", loc)
	}
}

// TestAdminCreateUser_InvalidUsernameRejected verifies malformed usernames are rejected.
func TestAdminCreateUser_InvalidUsernameRejected(t *testing.T) {
	ts, st := newTestServer(t)
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)

	resp := post(t, admin, ts.URL+"/admin/users/create",
		url.Values{"username": {"a b!"}, "role": {"engineering"}})
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if !strings.Contains(loc, "error=") {
		t.Fatalf("expected error redirect for invalid username, got %q", loc)
	}
}

// TestAdminCreateUser_EngineerForbidden verifies only admins can create accounts.
func TestAdminCreateUser_EngineerForbidden(t *testing.T) {
	ts, st := newTestServer(t)
	_ = provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	eng := provisionAndLogin(t, ts, st, "dev", store.RoleEngineering)

	resp := post(t, eng, ts.URL+"/admin/users/create",
		url.Values{"username": {"x"}, "role": {"engineering"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("engineer create user: want 403, got %d", resp.StatusCode)
	}
}

// TestAdminResetPassword_ForcesReset verifies an admin-triggered password
// reset issues a new temp password and forces the user through reset again.
func TestAdminResetPassword_ForcesReset(t *testing.T) {
	ts, st := newTestServer(t)
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	_ = provisionAndLogin(t, ts, st, "dev", store.RoleEngineering) // completes initial reset

	target, _ := st.UserByUsername("dev")
	resp := post(t, admin, ts.URL+"/admin/users/reset-password",
		url.Values{"id": {itoa(target.ID)}})
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if !strings.Contains(loc, "newpass=") {
		t.Fatalf("expected a new temp password in redirect, got %q", loc)
	}

	got, _ := st.UserByUsername("dev")
	if !got.MustResetPassword {
		t.Fatal("admin reset must force must_reset_password again")
	}
}

func extractQueryParam(t *testing.T, rawURL, key string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get(key)
}
