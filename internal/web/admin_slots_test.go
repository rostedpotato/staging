package web

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"parkee/staging-platform/internal/store"
)

func body(t *testing.T, c *http.Client, u string) string {
	t.Helper()
	r, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

func TestAdminSlots_RBAC(t *testing.T) {
	ts, st := newTestServer(t, "ho", "dev-09")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	eng := provisionAndLogin(t, ts, st, "dev", store.RoleEngineering)

	r, _ := admin.Get(ts.URL + "/admin/slots")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("admin /admin/slots: want 200, got %d", r.StatusCode)
	}
	r.Body.Close()

	r, _ = eng.Get(ts.URL + "/admin/slots")
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("eng /admin/slots: want 403, got %d", r.StatusCode)
	}
	r.Body.Close()
}

func TestAdminSlots_HideRemovesFromViews(t *testing.T) {
	ts, st := newTestServer(t, "ho", "dev-09")
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)

	// Both slots visible initially on the dashboard.
	dash := body(t, admin, ts.URL+"/")
	if !strings.Contains(dash, ">ho<") || !strings.Contains(dash, ">dev-09<") {
		t.Fatalf("expected both slots on dashboard initially")
	}

	// Hide dev-09.
	resp := post(t, admin, ts.URL+"/admin/slots/toggle",
		url.Values{"slot": {"dev-09"}, "hidden": {"1"}})
	resp.Body.Close()
	if h, _ := st.HiddenSlots(); !h["dev-09"] {
		t.Fatal("dev-09 should be hidden in store")
	}

	// Dashboard no longer lists dev-09 (ho remains).
	dash = body(t, admin, ts.URL+"/")
	if strings.Contains(dash, ">dev-09<") {
		t.Fatal("hidden slot dev-09 still on dashboard")
	}
	if !strings.Contains(dash, ">ho<") {
		t.Fatal("visible slot ho missing from dashboard")
	}

	// Booking + deploy option lists exclude dev-09 too.
	bookings := body(t, admin, ts.URL+"/bookings")
	if strings.Contains(bookings, `value="dev-09"`) {
		t.Fatal("hidden slot dev-09 still selectable in bookings")
	}
	deploy := body(t, admin, ts.URL+"/deploy")
	if strings.Contains(deploy, `value="dev-09"`) {
		t.Fatal("hidden slot dev-09 still selectable in deploy")
	}

	// Show it again -> reappears on dashboard.
	resp = post(t, admin, ts.URL+"/admin/slots/toggle",
		url.Values{"slot": {"dev-09"}, "hidden": {"0"}})
	resp.Body.Close()
	dash = body(t, admin, ts.URL+"/")
	if !strings.Contains(dash, ">dev-09<") {
		t.Fatal("shown slot dev-09 missing from dashboard after unhide")
	}
}
