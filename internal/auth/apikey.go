package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"parkee/staging-platform/internal/config"
)

// APIPrincipal is the authenticated holder of an API key.
type APIPrincipal struct {
	Name   string
	Scopes []string
}

// HasScope reports whether the key was granted scope.
func (p *APIPrincipal) HasScope(scope string) bool {
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

const principalKey ctxKey = 2

// PrincipalFrom returns the API key holder placed on the context by
// RequireAPIKey, or nil.
func PrincipalFrom(ctx context.Context) *APIPrincipal {
	p, _ := ctx.Value(principalKey).(*APIPrincipal)
	return p
}

// HashAPIKey returns the config representation ("sha256:<hex>") of a raw key.
func HashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// APIKeyLookup resolves a raw key to its principal. It is the seam for moving
// keys from config to a database later: RequireAPIKey only depends on this.
type APIKeyLookup interface {
	Lookup(rawKey string) (*APIPrincipal, bool)
}

// ConfigKeys looks keys up in the static config. Every configured hash is
// compared in constant time and the loop never exits early, so response
// timing does not reveal which (or whether any) key matched.
type ConfigKeys struct {
	keys []config.APIKey
}

func NewConfigKeys(keys []config.APIKey) *ConfigKeys { return &ConfigKeys{keys: keys} }

func (c *ConfigKeys) Lookup(rawKey string) (*APIPrincipal, bool) {
	got := []byte(HashAPIKey(rawKey))
	var found *APIPrincipal
	for i := range c.keys {
		if subtle.ConstantTimeCompare(got, []byte(c.keys[i].Hash)) == 1 {
			found = &APIPrincipal{Name: c.keys[i].Name, Scopes: c.keys[i].Scopes}
		}
	}
	return found, found != nil
}

// RequireAPIKey guards machine-facing endpoints. It accepts only
// "Authorization: Bearer <key>" - never the session cookie, so these
// endpoints cannot be reached through a browser's ambient credentials (no CSRF
// surface). Failures are JSON, not a redirect to /login.
func RequireAPIKey(lookup APIKeyLookup, scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
			WriteAPIError(w, http.StatusUnauthorized, "unauthorized", "missing or malformed Authorization header; expected 'Bearer <api key>'")
			return
		}
		p, ok := lookup.Lookup(raw)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="api", error="invalid_token"`)
			WriteAPIError(w, http.StatusUnauthorized, "unauthorized", "invalid API key")
			return
		}
		if !p.HasScope(scope) {
			WriteAPIError(w, http.StatusForbidden, "forbidden", "API key lacks required scope: "+scope)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	return tok, tok != ""
}

// WriteAPIError writes the uniform error body used by every /api/v1 endpoint:
// {"error":{"code":"...","message":"..."}}.
func WriteAPIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}
