package web

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"parkee/staging-platform/internal/auth"
	"parkee/staging-platform/internal/store"
)

var usernameRe = regexp.MustCompile(`^[a-z0-9._-]{3,32}$`)

type adminUsersPage struct {
	Username string
	IsAdmin  bool
	Users    []store.User
	Notice   string
	Error    string
	SelfID   int64
	// NewPassword/NewPasswordFor show a just-generated temp password once.
	NewPassword    string
	NewPasswordFor string
}

func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	users, _ := s.st.ListUsers()
	s.render(w, "admin_users.html", adminUsersPage{
		Username:       u.Username,
		IsAdmin:        true, // route is admin-only
		Users:          users,
		SelfID:         u.ID,
		Notice:         r.URL.Query().Get("notice"),
		Error:          r.URL.Query().Get("error"),
		NewPassword:    r.URL.Query().Get("newpass"),
		NewPasswordFor: r.URL.Query().Get("newpassfor"),
	})
}

// handleAdminUserCreate provisions a new account with a random temp password.
// This is the ONLY way an account comes into existence (besides the one-time
// bootstrap admin) — logging in never creates a user.
func (s *Server) handleAdminUserCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	actor := auth.UserFrom(r.Context())
	username := strings.ToLower(strings.TrimSpace(r.FormValue("username")))
	name := strings.TrimSpace(r.FormValue("name"))
	role := r.FormValue("role")

	if !usernameRe.MatchString(username) {
		redirectTo(w, r, "/admin/users", "", "username must be 3-32 chars: lowercase letters, digits, dot, underscore, hyphen")
		return
	}
	if role != store.RoleAdmin && role != store.RoleEngineering {
		role = store.RoleEngineering
	}

	temp, err := store.GenerateTempPassword()
	if err != nil {
		http.Error(w, "could not generate password", http.StatusInternalServerError)
		return
	}
	created, err := s.st.CreateUser(username, name, role, temp)
	if err != nil {
		if err == store.ErrUserExists {
			redirectTo(w, r, "/admin/users", "", "a user with that username already exists")
			return
		}
		redirectTo(w, r, "/admin/users", "", err.Error())
		return
	}
	_ = s.st.Audit(actor.ID, actor.Username, "CREATE_USER", "user", strconv.FormatInt(created.ID, 10),
		map[string]any{"username": created.Username, "role": created.Role})

	redirectWithPassword(w, r, "Created account for "+created.Username+".", created.Username, temp)
}

// handleAdminUserResetPassword generates a fresh temp password for an
// existing account and forces them to change it on next login.
func (s *Server) handleAdminUserResetPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	actor := auth.UserFrom(r.Context())
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	target, err := s.st.UserByID(id)
	if err != nil || target == nil {
		redirectTo(w, r, "/admin/users", "", "user not found")
		return
	}
	temp, err := s.st.ResetPassword(id)
	if err != nil {
		redirectTo(w, r, "/admin/users", "", err.Error())
		return
	}
	_ = s.st.Audit(actor.ID, actor.Username, "RESET_PASSWORD", "user", strconv.FormatInt(id, 10),
		map[string]any{"username": target.Username})
	redirectWithPassword(w, r, "Password reset for "+target.Username+".", target.Username, temp)
}

func redirectWithPassword(w http.ResponseWriter, r *http.Request, notice, username, temp string) {
	q := "?notice=" + urlValue(notice) + "&newpass=" + urlValue(temp) + "&newpassfor=" + urlValue(username)
	http.Redirect(w, r, "/admin/users"+q, http.StatusSeeOther)
}

func (s *Server) handleAdminUserRole(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	actor := auth.UserFrom(r.Context())
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	role := r.FormValue("role")
	if role != store.RoleAdmin && role != store.RoleEngineering {
		redirectTo(w, r, "/admin/users", "", "invalid role")
		return
	}
	target, err := s.st.UserByID(id)
	if err != nil || target == nil {
		redirectTo(w, r, "/admin/users", "", "user not found")
		return
	}
	// Guard: don't remove the last admin.
	if target.Role == store.RoleAdmin && role == store.RoleEngineering {
		if n, _ := s.st.CountAdmins(); n <= 1 {
			redirectTo(w, r, "/admin/users", "", "cannot demote the last admin")
			return
		}
	}
	if err := s.st.SetRole(id, role); err != nil {
		redirectTo(w, r, "/admin/users", "", err.Error())
		return
	}
	_ = s.st.Audit(actor.ID, actor.Username, "SET_ROLE", "user", strconv.FormatInt(id, 10),
		map[string]any{"username": target.Username, "role": role})
	redirectTo(w, r, "/admin/users", "Updated role for "+target.Username+".", "")
}

func (s *Server) handleAdminUserStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	actor := auth.UserFrom(r.Context())
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	status := r.FormValue("status")
	if status != "active" && status != "disabled" {
		redirectTo(w, r, "/admin/users", "", "invalid status")
		return
	}
	target, err := s.st.UserByID(id)
	if err != nil || target == nil {
		redirectTo(w, r, "/admin/users", "", "user not found")
		return
	}
	// Guard: don't disable yourself or the last admin.
	if status == "disabled" {
		if target.ID == actor.ID {
			redirectTo(w, r, "/admin/users", "", "you cannot disable your own account")
			return
		}
		if target.Role == store.RoleAdmin {
			if n, _ := s.st.CountAdmins(); n <= 1 {
				redirectTo(w, r, "/admin/users", "", "cannot disable the last admin")
				return
			}
		}
	}
	if err := s.st.SetStatus(id, status); err != nil {
		redirectTo(w, r, "/admin/users", "", err.Error())
		return
	}
	_ = s.st.Audit(actor.ID, actor.Username, "SET_STATUS", "user", strconv.FormatInt(id, 10),
		map[string]any{"username": target.Username, "status": status})
	redirectTo(w, r, "/admin/users", "Updated status for "+target.Username+".", "")
}

type slotRow struct {
	Name   string
	Hidden bool
}

type adminSlotsPage struct {
	Username string
	IsAdmin  bool
	Slots    []slotRow
	Notice   string
	Error    string
}

func (s *Server) handleAdminSlots(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	u := auth.UserFrom(r.Context())
	hidden, _ := s.st.HiddenSlots()
	var rows []slotRow
	for _, name := range s.allSlotNames(ctx) {
		rows = append(rows, slotRow{Name: name, Hidden: hidden[name] || s.cfg.Hidden(name)})
	}
	s.render(w, "admin_slots.html", adminSlotsPage{
		Username: u.Username,
		IsAdmin:  true, // route is admin-only
		Slots:    rows,
		Notice:   r.URL.Query().Get("notice"),
		Error:    r.URL.Query().Get("error"),
	})
}

func (s *Server) handleAdminSlotToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/admin/slots", http.StatusSeeOther)
		return
	}
	actor := auth.UserFrom(r.Context())
	slot := r.FormValue("slot")
	hide := r.FormValue("hidden") == "1"
	if slot == "" {
		redirectTo(w, r, "/admin/slots", "", "slot required")
		return
	}
	if err := s.st.SetSlotHidden(slot, hide); err != nil {
		redirectTo(w, r, "/admin/slots", "", err.Error())
		return
	}
	action := "SHOW_SLOT"
	if hide {
		action = "HIDE_SLOT"
	}
	_ = s.st.Audit(actor.ID, actor.Username, action, "slot", slot, nil)
	redirectTo(w, r, "/admin/slots", "Updated "+slot+".", "")
}
