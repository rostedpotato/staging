package auth

import (
	"net/http"

	"parkee/staging-platform/internal/store"
)

// Routes registers auth endpoints on mux.
func (a *Authenticator) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/login", a.handleLoginPage)
	mux.HandleFunc("/auth/login", a.handleLoginSubmit)
	mux.HandleFunc("/logout", a.handleLogout)
	mux.Handle("/reset-password", a.RequireUser(http.HandlerFunc(a.handleResetPassword)))
}

func (a *Authenticator) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if UserFrom(r.Context()) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.renderLogin(w, "")
}

// handleLoginSubmit verifies username+password. Accounts are NOT created by
// logging in: an admin must create the account first via /admin/users. The
// one exception is bootstrap — if the system has no users at all, this never
// applies (bootstrap itself happens via CreateUser at first-run, see main).
func (a *Authenticator) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	username := normalizeUsername(r.FormValue("username"))
	password := r.FormValue("password")
	if username == "" || password == "" {
		a.renderLogin(w, "Enter your username and password.")
		return
	}

	u, err := a.st.VerifyPassword(username, password)
	if err != nil {
		if err == store.ErrInvalidCredentials {
			a.renderLogin(w, "Invalid username or password.")
			return
		}
		http.Error(w, "login error", http.StatusInternalServerError)
		return
	}
	if err := a.setSession(w, u); err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	_ = a.st.Audit(u.ID, u.Username, "LOGIN", "session", "", map[string]any{"role": u.Role})
	if u.MustResetPassword {
		http.Redirect(w, r, "/reset-password", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleResetPassword lets a logged-in user set their own password. It is
// mandatory after an admin-provisioned account or an admin-triggered reset.
func (a *Authenticator) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	if r.Method != http.MethodPost {
		a.renderResetPassword(w, u, "")
		return
	}
	newPass := r.FormValue("new_password")
	confirm := r.FormValue("confirm_password")
	if len(newPass) < 8 {
		a.renderResetPassword(w, u, "Password must be at least 8 characters.")
		return
	}
	if newPass != confirm {
		a.renderResetPassword(w, u, "Passwords do not match.")
		return
	}
	if err := a.st.SetPassword(u.ID, newPass); err != nil {
		http.Error(w, "could not set password", http.StatusInternalServerError)
		return
	}
	_ = a.st.Audit(u.ID, u.Username, "SET_OWN_PASSWORD", "user", "", nil)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *Authenticator) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.st.DeleteSession(c.Value)
		if u := UserFrom(r.Context()); u != nil {
			_ = a.st.Audit(u.ID, u.Username, "LOGOUT", "session", "", nil)
		}
	}
	a.clearCookie(w, sessionCookie)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
