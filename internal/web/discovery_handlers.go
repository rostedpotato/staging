package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"parkee/staging-platform/internal/auth"
	"parkee/staging-platform/internal/store"
)

type discoveryPage struct {
	Username string
	IsAdmin  bool
	Notice   string
	Error    string
	// Draft is the run waiting for review (or still running), if any.
	Draft  *store.DiscoveryRun
	Recent []store.DiscoveryRun
	Envs   []store.Environment
	// AutomationHost is shown so the admin knows where SSH goes.
	AutomationHost string
}

func (s *Server) handleDiscoveryPage(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	recent, _ := s.st.RecentDiscoveryRuns(10)
	var draft *store.DiscoveryRun
	for i := range recent {
		if recent[i].Status == store.RunRunning || recent[i].Status == store.RunReady {
			draft = &recent[i]
			break
		}
	}
	envs, _ := s.st.ListEnvironments("")
	s.render(w, "admin_discovery.html", discoveryPage{
		Username:       u.Username,
		IsAdmin:        true,
		Notice:         r.URL.Query().Get("notice"),
		Error:          r.URL.Query().Get("error"),
		Draft:          draft,
		Recent:         recent,
		Envs:           envs,
		AutomationHost: s.cfg.Automation.Host,
	})
}

// handleDiscoveryRun starts a discovery in the background and returns at
// once; the page shows "running" until the result is ready for review.
func (s *Server) handleDiscoveryRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/admin/discovery", http.StatusSeeOther)
		return
	}
	u := auth.UserFrom(r.Context())
	var servers []string
	switch r.FormValue("servers") {
	case store.ServerStaging:
		servers = []string{store.ServerStaging}
	case store.ServerAutomation:
		servers = []string{store.ServerAutomation}
	default:
		servers = []string{store.ServerStaging, store.ServerAutomation}
	}
	id, err := s.st.StartDiscoveryRun(u.Username, servers)
	if errors.Is(err, store.ErrRunInProgress) {
		redirectTo(w, r, "/admin/discovery", "", "Discovery is already running or waiting for review. Save or discard it first.")
		return
	}
	if err != nil {
		redirectTo(w, r, "/admin/discovery", "", err.Error())
		return
	}
	_ = s.st.Audit(u.ID, u.Username, "RUN_DISCOVERY", "discovery", strconv.FormatInt(id, 10),
		map[string]any{"servers": servers})
	go s.runDiscovery(id, servers)
	redirectTo(w, r, "/admin/discovery", "Discovery started. Refresh this page in a moment.", "")
}

// runDiscovery runs docker ps on each requested server and stores the diff
// against the current inventory as a draft. A server that fails is reported
// but never causes its envs to be marked missing.
func (s *Server) runDiscovery(id int64, servers []string) {
	s.discoverMu.Lock()
	defer s.discoverMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	result := store.DiscoveryResult{Errors: map[string]string{}}
	var found []store.EnvChange
	scanned := map[string]bool{}
	for _, server := range servers {
		var envs []store.EnvChange
		var err error
		if server == store.ServerStaging {
			envs, err = s.disc.DiscoverStaging(ctx)
		} else {
			envs, err = s.disc.DiscoverAutomation(ctx, s.cfg.Automation)
		}
		if err != nil {
			log.Printf("discovery %d: %s: %v", id, server, err)
			result.Errors[server] = err.Error()
			continue
		}
		scanned[server] = true
		found = append(found, envs...)
	}
	stored, err := s.st.ListEnvironments("")
	if err != nil {
		result.Errors["store"] = err.Error()
	}
	result.Changes = store.DiffEnvironments(stored, found, scanned)
	if err := s.st.FinishDiscoveryRun(id, result, len(scanned) == 0); err != nil {
		log.Printf("discovery %d: save result: %v", id, err)
	}
}

// handleDiscoveryDecide applies or discards a reviewed draft.
func (s *Server) handleDiscoveryDecide(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/admin/discovery", http.StatusSeeOther)
		return
	}
	u := auth.UserFrom(r.Context())
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	run, err := s.st.DiscoveryRunByID(id)
	if err != nil || run == nil {
		redirectTo(w, r, "/admin/discovery", "", "discovery run not found")
		return
	}
	apply := r.FormValue("action") == "apply"
	status := store.RunDiscarded
	if apply {
		status = store.RunApplied
	}
	ok, err := s.st.DecideDiscoveryRun(id, status, u.Username)
	if err != nil {
		redirectTo(w, r, "/admin/discovery", "", err.Error())
		return
	}
	if !ok {
		redirectTo(w, r, "/admin/discovery", "", "this run was already decided or is still running")
		return
	}
	if !apply {
		_ = s.st.Audit(u.ID, u.Username, "DISCARD_DISCOVERY", "discovery", strconv.FormatInt(id, 10), nil)
		redirectTo(w, r, "/admin/discovery", "Discovery discarded. Nothing changed.", "")
		return
	}

	changes := pendingChanges(run.Result.Changes)
	if err := s.st.ApplyEnvChanges(changes); err != nil {
		redirectTo(w, r, "/admin/discovery", "", "apply failed: "+err.Error())
		return
	}
	_ = s.st.Audit(u.ID, u.Username, "APPLY_DISCOVERY", "discovery", strconv.FormatInt(id, 10),
		map[string]any{"changes": len(changes)})
	go s.refreshStatus(context.Background())
	redirectTo(w, r, "/admin/discovery", fmt.Sprintf("Saved %d change(s). Status will refresh in a few seconds.", len(changes)), "")
}

// pendingChanges drops "unchanged" rows, which need no write.
func pendingChanges(all []store.EnvChange) []store.EnvChange {
	var out []store.EnvChange
	for _, c := range all {
		if c.Kind != "unchanged" {
			out = append(out, c)
		}
	}
	return out
}
