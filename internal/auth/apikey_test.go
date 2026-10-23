package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"parkee/staging-platform/internal/config"
)

func testHandler(lookup APIKeyLookup, scope string) http.Handler {
	return RequireAPIKey(lookup, scope, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := PrincipalFrom(r.Context())
		w.Write([]byte("hello " + p.Name))
	}))
}

func testKeys() *ConfigKeys {
	return NewConfigKeys([]config.APIKey{
		{Name: "bi", Hash: HashAPIKey("key-bi"), Scopes: []string{"bookings:read"}},
		{Name: "other", Hash: HashAPIKey("key-other"), Scopes: []string{"deploy:write"}},
	})
}

func do(h http.Handler, authz string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/api/v1/x", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("content-type = %q, want JSON", ct)
	}
	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, rec.Body.String())
	}
	return body.Error.Code
}

func TestRequireAPIKey(t *testing.T) {
	h := testHandler(testKeys(), "bookings:read")

	t.Run("valid key", func(t *testing.T) {
		rec := do(h, "Bearer key-bi")
		if rec.Code != 200 || rec.Body.String() != "hello bi" {
			t.Fatalf("got %d %q", rec.Code, rec.Body.String())
		}
	})
	t.Run("scheme is case-insensitive", func(t *testing.T) {
		if rec := do(h, "bearer key-bi"); rec.Code != 200 {
			t.Fatalf("got %d", rec.Code)
		}
	})
	t.Run("missing header", func(t *testing.T) {
		rec := do(h, "")
		if rec.Code != 401 || errCode(t, rec) != "unauthorized" {
			t.Fatalf("got %d", rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Fatal("want WWW-Authenticate header")
		}
	})
	t.Run("wrong scheme", func(t *testing.T) {
		if rec := do(h, "Basic key-bi"); rec.Code != 401 {
			t.Fatalf("got %d", rec.Code)
		}
	})
	t.Run("empty token", func(t *testing.T) {
		if rec := do(h, "Bearer "); rec.Code != 401 {
			t.Fatalf("got %d", rec.Code)
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		rec := do(h, "Bearer nope")
		if rec.Code != 401 || errCode(t, rec) != "unauthorized" {
			t.Fatalf("got %d", rec.Code)
		}
	})
	t.Run("valid key without scope", func(t *testing.T) {
		rec := do(h, "Bearer key-other")
		if rec.Code != 403 || errCode(t, rec) != "forbidden" {
			t.Fatalf("got %d", rec.Code)
		}
	})
	t.Run("session cookie alone is rejected", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/x", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "whatever"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatalf("got %d, want 401", rec.Code)
		}
	})
	t.Run("no keys configured rejects everything", func(t *testing.T) {
		empty := testHandler(NewConfigKeys(nil), "bookings:read")
		if rec := do(empty, "Bearer key-bi"); rec.Code != 401 {
			t.Fatalf("got %d", rec.Code)
		}
	})
}

func TestHashAPIKeyFormat(t *testing.T) {
	// sha256("abc")
	want := "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := HashAPIKey("abc"); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}
