package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"parkee/staging-platform/internal/store"
)

func TestAudit_RBAC(t *testing.T) {
	ts, st := newTestServer(t)
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	eng := provisionAndLogin(t, ts, st, "dev", store.RoleEngineering)

	r, _ := admin.Get(ts.URL + "/audit")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("admin /audit: want 200, got %d", r.StatusCode)
	}
	r.Body.Close()

	r, _ = eng.Get(ts.URL + "/audit")
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("eng /audit: want 403, got %d", r.StatusCode)
	}
	r.Body.Close()
}

func TestAudit_Search(t *testing.T) {
	ts, st := newTestServer(t)
	admin := provisionAndLogin(t, ts, st, "boss", store.RoleAdmin)
	_ = provisionAndLogin(t, ts, st, "dev", store.RoleEngineering)

	// Generate an auditable action: promote dev -> admin (writes SET_ROLE).
	dev, _ := st.UserByUsername("dev")
	post(t, admin, ts.URL+"/admin/users/role",
		url.Values{"id": {itoa(dev.ID)}, "role": {"admin"}}).Body.Close()

	// Search matches the SET_ROLE action.
	page := body(t, admin, ts.URL+"/audit?q=SET_ROLE")
	if !strings.Contains(page, "SET_ROLE") {
		t.Fatal("search q=SET_ROLE should list the SET_ROLE entry")
	}
	if !strings.Contains(page, "dev") {
		t.Fatal("SET_ROLE entry should reference dev in metadata")
	}

	// Search by actor email returns their LOGIN entries.
	page = body(t, admin, ts.URL+"/audit?q=boss")
	if !strings.Contains(page, "LOGIN") {
		t.Fatal("search by actor should list their LOGIN entry")
	}

	// A query that matches nothing yields no rows.
	page = body(t, admin, ts.URL+"/audit?q=zzz_no_match_zzz")
	if strings.Contains(page, "SET_ROLE") || strings.Contains(page, "LOGIN") {
		t.Fatal("non-matching query should return no audit rows")
	}
}
