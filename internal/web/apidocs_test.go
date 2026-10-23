package web

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
)

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData(openAPISpec())
	if err != nil {
		t.Fatalf("openapi.yaml does not parse: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("openapi.yaml is not a valid OpenAPI document: %v", err)
	}
	return doc
}

func TestOpenAPISpecIsValid(t *testing.T) {
	doc := loadSpec(t)
	if doc.Components == nil || doc.Components.SecuritySchemes["bearerAuth"] == nil {
		t.Fatal("bearerAuth security scheme missing")
	}
}

// The code (apiEndpoints) and the document must describe exactly the same
// operations, with the same required scope. Adding an endpoint without
// documenting it - or documenting one that does not exist - fails here.
func TestOpenAPISpecMatchesRoutes(t *testing.T) {
	doc := loadSpec(t)
	srv, _, _ := newTestServerFull(t, nil)

	code := map[string]string{} // "METHOD path" -> scope
	for _, e := range srv.apiEndpoints() {
		code[e.Method+" "+e.Path] = e.Scope
	}
	spec := map[string]string{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			scope, _ := op.Extensions["x-required-scope"].(string)
			spec[method+" "+path] = scope
		}
	}
	for k, scope := range code {
		got, ok := spec[k]
		if !ok {
			t.Errorf("route %q is implemented but not documented in openapi.yaml", k)
		} else if got != scope {
			t.Errorf("route %q: code requires scope %q, spec says x-required-scope %q", k, scope, got)
		}
	}
	for k := range spec {
		if _, ok := code[k]; !ok {
			t.Errorf("openapi.yaml documents %q but no such route is registered", k)
		}
	}
}

// A real response must satisfy the documented schema, so the examples and the
// DTO cannot drift apart.
func TestAPIBookingsResponseMatchesSchema(t *testing.T) {
	doc := loadSpec(t)
	base, st := apiTestServer(t, "dev-01")
	a := mustBook(t, st, "dev-01", "ana", "feature x", apiDay(2031, 8, 1), apiDay(2031, 8, 9))
	mustBook(t, st, "dev-01", "bob", "later", apiDay(2031, 8, 10), apiDay(2031, 8, 12))
	if err := st.Release(a.ID, "released"); err != nil { // exercises nullable releasedAt
		t.Fatal(err)
	}

	cases := []struct {
		query  string
		status int
	}{
		{"from=2031-08-01&to=2031-08-31", 200},
		{"from=2031-09-01&to=2031-09-30", 200}, // empty page
		{"from=2031-08-31&to=2031-08-01", 400},
		{"", 400},
	}
	doc.Servers = nil // match on path only; the test server's host is random
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		req, _ := http.NewRequest("GET", base+"/api/v1/bookings?"+c.query, nil)
		req.Header.Set("Authorization", "Bearer "+apiTestKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.status {
			t.Fatalf("%q: status %d want %d (%s)", c.query, resp.StatusCode, c.status, body)
		}

		route, pathParams, err := router.FindRoute(req)
		if err != nil {
			t.Fatalf("spec has no route for request: %v", err)
		}
		if err := validateResponse(t, route, pathParams, req, resp, body); err != nil {
			t.Errorf("%q: response violates openapi.yaml: %v\n%s", c.query, err, body)
		}
	}
}

func TestAPIDocsServed(t *testing.T) {
	ts, _ := newTestServer(t)
	get := func(path string) (*http.Response, string) {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	// Public: no key and no session needed.
	resp, body := get("/api/openapi.yaml")
	if resp.StatusCode != 200 || !strings.HasPrefix(body, "openapi: 3.") {
		t.Fatalf("openapi.yaml: %d %.40q", resp.StatusCode, body)
	}
	resp, body = get("/api/docs")
	if resp.StatusCode != 200 || !strings.Contains(body, "swagger-ui") {
		t.Fatalf("docs page: %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatal("docs page must carry a CSP restricting scripts to same-origin")
	}
	// Self-contained: the page must not pull anything from another origin.
	if strings.Contains(body, "http://") || strings.Contains(body, "https://") {
		t.Fatal("docs page references an external URL; assets must be vendored")
	}
	for _, p := range []string{"/api/docs/init.js", "/api/docs/assets/swagger-ui.css", "/api/docs/assets/swagger-ui-bundle.js"} {
		if r, b := get(p); r.StatusCode != 200 || len(b) == 0 {
			t.Fatalf("%s: %d", p, r.StatusCode)
		}
	}
	if r, _ := get("/api/docs/assets/../openapi.yaml"); r.StatusCode >= 500 {
		t.Fatalf("path traversal attempt: %d", r.StatusCode)
	}
}

// validateResponse checks a live response (status, headers, JSON body)
// against the operation's documented response schema.
func validateResponse(t *testing.T, route *routers.Route, pathParams map[string]string,
	req *http.Request, resp *http.Response, body []byte) error {
	t.Helper()
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request:    req,
			PathParams: pathParams,
			Route:      route,
			Options:    &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		},
		Status:  resp.StatusCode,
		Header:  resp.Header,
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}
	in.SetBodyBytes(body)
	return openapi3filter.ValidateResponse(context.Background(), in)
}
