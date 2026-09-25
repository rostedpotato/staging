package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"parkee/staging-platform/internal/auth"
	"parkee/staging-platform/internal/config"
	"parkee/staging-platform/internal/deploy"
	"parkee/staging-platform/internal/store"
)

type deployPage struct {
	Username string
	IsAdmin  bool
	Enabled  bool
	Slots    []string
	Services []config.DeployService
	Recent   []store.Deployment
	Error    string
	Notice   string
}

func (s *Server) handleDeployPage(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	recent, _ := s.st.ListDeployments("", 20)
	s.render(w, "deploy.html", deployPage{
		Username: u.Username,
		IsAdmin:  u.IsAdmin(),
		Enabled:  s.cfg.Deploy.Enabled,
		Slots:    s.slotNames(),
		Services: s.cfg.Deploy.Services,
		Recent:   recent,
		Notice:   r.URL.Query().Get("notice"),
		Error:    r.URL.Query().Get("error"),
	})
}

func (s *Server) handleDeployStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/deploy", http.StatusSeeOther)
		return
	}
	u := auth.UserFrom(r.Context())
	branch := strings.TrimSpace(r.FormValue("branch"))
	tag := strings.TrimSpace(r.FormValue("tag"))
	// Enforce mutual exclusivity server-side too (the UI also disables one).
	if branch != "" && tag != "" {
		redirectTo(w, r, "/deploy", "", "provide either a branch or a tag, not both")
		return
	}
	refType, ref := "branch", branch
	if tag != "" {
		refType, ref = "tag", tag
	}
	req := deploy.Request{
		Slot:     r.FormValue("slot"),
		Service:  r.FormValue("service"),
		RefType:  refType,
		Ref:      ref,
		UserID:   u.ID,
		Username: u.Username,
		IsAdmin:  u.IsAdmin(),
	}
	dep, err := s.deploy.Start(r.Context(), req)
	if err != nil {
		redirectTo(w, r, "/deploy", "", err.Error())
		return
	}
	_ = s.st.Audit(u.ID, u.Username, "DEPLOY", "deployment", strconv.FormatInt(dep.ID, 10),
		map[string]any{"slot": req.Slot, "service": req.Service, "refType": req.RefType, "ref": req.Ref})
	http.Redirect(w, r, fmt.Sprintf("/deployments/%d", dep.ID), http.StatusSeeOther)
}

func (s *Server) handleDeployRollback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/deployments", http.StatusSeeOther)
		return
	}
	u := auth.UserFrom(r.Context())
	// Rollback is an elevated action -> admin only.
	if !u.IsAdmin() {
		http.Error(w, "forbidden: admin only", http.StatusForbidden)
		return
	}
	fromID, _ := strconv.ParseInt(r.FormValue("from"), 10, 64)
	dep, err := s.deploy.Rollback(r.Context(), fromID, u.ID, u.Username, u.IsAdmin())
	if err != nil {
		redirectTo(w, r, fmt.Sprintf("/deployments/%d", fromID), "", err.Error())
		return
	}
	_ = s.st.Audit(u.ID, u.Username, "ROLLBACK", "deployment", strconv.FormatInt(dep.ID, 10),
		map[string]any{"from": fromID, "slot": dep.Slot, "service": dep.Service, "ref": dep.Ref})
	http.Redirect(w, r, fmt.Sprintf("/deployments/%d", dep.ID), http.StatusSeeOther)
}

func (s *Server) handleDeployHistory(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	slot := r.URL.Query().Get("slot")
	deps, _ := s.st.ListDeployments(slot, 100)
	s.render(w, "deployments.html", struct {
		Username    string
		IsAdmin     bool
		Deployments []store.Deployment
		Slot        string
	}{u.Username, u.IsAdmin(), deps, slot})
}

func (s *Server) handleDeployDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r.URL.Path, "/deployments/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	dep, err := s.st.DeploymentByID(id)
	if err != nil || dep == nil {
		http.NotFound(w, r)
		return
	}
	u := auth.UserFrom(r.Context())
	logs, _ := s.st.LogsSince(id, 0)
	var b strings.Builder
	for _, l := range logs {
		b.WriteString(l.Line)
		b.WriteByte('\n')
	}
	s.render(w, "deployment_detail.html", struct {
		Username string
		IsAdmin  bool
		Dep      *store.Deployment
		Log      string
		Active   bool
	}{u.Username, u.IsAdmin(), dep, b.String(), dep.Active()})
}

// handleDeployStream streams new log lines + status via Server-Sent Events.
func (s *Server) handleDeployStream(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r.URL.Path, "/deployments/stream/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx := r.Context()
	sinceSeq := 0
	if v := r.URL.Query().Get("since"); v != "" {
		sinceSeq, _ = strconv.Atoi(v)
	}
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()

	for {
		lines, _ := s.st.LogsSince(id, sinceSeq)
		for _, l := range lines {
			fmt.Fprintf(w, "event: log\ndata: %s\n\n", sseEscape(l.Line))
			sinceSeq = l.Seq + 1
		}
		dep, err := s.st.DeploymentByID(id)
		if err == nil && dep != nil && !dep.Active() {
			fmt.Fprintf(w, "event: status\ndata: %s\n\n", dep.Status)
			flusher.Flush()
			return
		}
		flusher.Flush()

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func redirectTo(w http.ResponseWriter, r *http.Request, path, notice, errMsg string) {
	q := ""
	if notice != "" {
		q = "?notice=" + urlValue(notice)
	} else if errMsg != "" {
		q = "?error=" + urlValue(errMsg)
	}
	http.Redirect(w, r, path+q, http.StatusSeeOther)
}

// idFromPath extracts a trailing integer id from path after prefix.
func idFromPath(path, prefix string) (int64, bool) {
	rest := strings.TrimPrefix(path, prefix)
	rest = strings.Trim(rest, "/")
	if rest == "" || strings.Contains(rest, "/") {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// sseEscape keeps a log line on a single SSE data field (newlines already split).
func sseEscape(s string) string {
	return strings.ReplaceAll(s, "\n", " ")
}

var _ = errors.New // reserved for future typed error handling
