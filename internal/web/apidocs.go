package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// The API documentation is fully self-contained (no CDN): the OpenAPI spec
// plus a vendored copy of Swagger UI (swagger-ui-dist, Apache-2.0; see
// apidocs/swagger-ui/LICENSE), so it works on staging hosts without internet.
//
//go:embed apidocs/openapi.yaml apidocs/index.html apidocs/init.js apidocs/swagger-ui/*
var apidocsFS embed.FS

// openAPISpec returns the embedded OpenAPI document.
func openAPISpec() []byte {
	b, err := apidocsFS.ReadFile("apidocs/openapi.yaml")
	if err != nil {
		panic(err) // embedded at build time; cannot happen
	}
	return b
}

func serveEmbedded(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := apidocsFS.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if strings.HasPrefix(contentType, "text/html") {
			// Scripts only from this origin; Swagger UI needs inline styles.
			w.Header().Set("Content-Security-Policy",
				"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'")
		}
		w.Write(b)
	}
}

// docsRoutes registers the public documentation endpoints. They expose no
// data and no secrets, so they need no API key: anyone can read how the API
// works, while calling it still requires a key.
func (s *Server) docsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/openapi.yaml", serveEmbedded("apidocs/openapi.yaml", "application/yaml; charset=utf-8"))
	mux.HandleFunc("/api/docs", serveEmbedded("apidocs/index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("/api/docs/init.js", serveEmbedded("apidocs/init.js", "text/javascript; charset=utf-8"))
	assets, _ := fs.Sub(apidocsFS, "apidocs/swagger-ui")
	mux.Handle("/api/docs/assets/", http.StripPrefix("/api/docs/assets/", http.FileServer(http.FS(assets))))
}
