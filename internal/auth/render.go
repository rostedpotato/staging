package auth

import (
	"embed"
	"html/template"
	"net/http"

	"parkee/staging-platform/internal/store"
)

//go:embed templates/*.html
var tmplFS embed.FS

var loginTmpl = template.Must(template.ParseFS(tmplFS, "templates/*.html"))

type loginData struct {
	Error string
}

func (a *Authenticator) renderLogin(w http.ResponseWriter, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if errMsg != "" {
		w.WriteHeader(http.StatusUnauthorized)
	}
	_ = loginTmpl.ExecuteTemplate(w, "login.html", loginData{Error: errMsg})
}

type resetPasswordData struct {
	Username string
	Error    string
}

func (a *Authenticator) renderResetPassword(w http.ResponseWriter, u *store.User, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := resetPasswordData{Error: errMsg}
	if u != nil {
		data.Username = u.Username
	}
	_ = loginTmpl.ExecuteTemplate(w, "reset_password.html", data)
}
