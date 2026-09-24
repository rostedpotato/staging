package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	"parkee/staging-platform/internal/config"
	"parkee/staging-platform/internal/store"
)

const sessionCookie = "sp_session"

type ctxKey int

const userKey ctxKey = 1

type Authenticator struct {
	cfg *config.Config
	st  *store.Store
}

func New(cfg *config.Config, st *store.Store) *Authenticator {
	return &Authenticator{cfg: cfg, st: st}
}

// --- request context helpers -------------------------------------------------

func UserFrom(ctx context.Context) *store.User {
	u, _ := ctx.Value(userKey).(*store.User)
	return u
}

// LoadUser resolves the session cookie to a user and puts it on the context.
func (a *Authenticator) LoadUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil {
			if u, err := a.st.UserForSession(c.Value); err == nil && u != nil {
				r = r.WithContext(context.WithValue(r.Context(), userKey, u))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// RequireUser redirects anonymous requests to the login page, and forces a
// user with a temporary password to the reset-password page first.
func (a *Authenticator) RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := UserFrom(r.Context())
		if u == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if u.MustResetPassword && r.URL.Path != "/reset-password" && r.URL.Path != "/logout" {
			http.Redirect(w, r, "/reset-password", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAdmin returns 403 for non-admins.
func (a *Authenticator) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := UserFrom(r.Context())
		if u == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if !u.IsAdmin() {
			http.Error(w, "forbidden: admin only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- session cookie ----------------------------------------------------------

func (a *Authenticator) setSession(w http.ResponseWriter, u *store.User) error {
	ttl := time.Duration(a.cfg.Auth.SessionTTLHours) * time.Hour
	sess, err := a.st.CreateSession(u.ID, ttl)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sess.ID,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cfg.Auth.Secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  sess.ExpiresAt,
	})
	return nil
}

func (a *Authenticator) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.cfg.Auth.Secure, SameSite: http.SameSiteLaxMode,
	})
}

func normalizeUsername(u string) string { return strings.ToLower(strings.TrimSpace(u)) }
