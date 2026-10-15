// Package web serves the dashboard, bookings, deploys and admin pages.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"parkee/staging-platform/internal/auth"
	"parkee/staging-platform/internal/config"
	"parkee/staging-platform/internal/deploy"
	"parkee/staging-platform/internal/discovery"
	"parkee/staging-platform/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

type Server struct {
	cfg    *config.Config
	disc   *discovery.Discoverer
	auth   *auth.Authenticator
	st     *store.Store
	deploy *deploy.Service
	tmpl   *template.Template

	// statusMu guards the cached status of every known env (staging and
	// automation). It's refreshed by a background loop (StartStatusLoop) that
	// only curls /actuator/info, so page loads never wait on it.
	statusMu         sync.RWMutex
	statusAt         time.Time
	stagingStatus    []discovery.Slot
	automationStatus []discovery.Slot

	// discoverMu makes sure only one admin-triggered discovery runs at a time.
	discoverMu sync.Mutex

	// powerMu guards power: the in-flight stop/start per env (see env_power.go).
	powerMu sync.Mutex
	power   map[string]*powerState
}

func NewServer(cfg *config.Config, disc *discovery.Discoverer, a *auth.Authenticator, st *store.Store, dep *deploy.Service) (*Server, error) {
	funcs := template.FuncMap{
		"fmtTime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Local().Format("2006-01-02 15:04")
		},
		"fmtDate": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Local().Format("2006-01-02")
		},
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, disc: disc, auth: a, st: st, deploy: dep, tmpl: tmpl,
		power: map[string]*powerState{}}, nil
}

func (s *Server) renderBookings(w http.ResponseWriter, data bookingsPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "bookings.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))

	// Public endpoints.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	s.auth.Routes(mux)

	// Protected endpoints (login required).
	mux.Handle("/api/slots", s.auth.RequireUser(http.HandlerFunc(s.handleAPISlots)))
	mux.Handle("/audit", s.auth.RequireAdmin(http.HandlerFunc(s.handleAudit)))
	mux.Handle("/bookings", s.auth.RequireUser(http.HandlerFunc(s.handleBookings)))
	mux.Handle("/bookings/create", s.auth.RequireUser(http.HandlerFunc(s.handleBookingCreate)))
	mux.Handle("/bookings/release", s.auth.RequireUser(http.HandlerFunc(s.handleBookingRelease)))
	mux.Handle("/deploy", s.auth.RequireUser(http.HandlerFunc(s.handleDeployPage)))
	mux.Handle("/deploy/start", s.auth.RequireUser(http.HandlerFunc(s.handleDeployStart)))
	mux.Handle("/deployments", s.auth.RequireUser(http.HandlerFunc(s.handleDeployHistory)))
	mux.Handle("/deployments/", s.auth.RequireUser(http.HandlerFunc(s.handleDeployDetail)))
	mux.Handle("/deployments/stream/", s.auth.RequireUser(http.HandlerFunc(s.handleDeployStream)))
	mux.Handle("/deploy/rollback", s.auth.RequireUser(http.HandlerFunc(s.handleDeployRollback)))

	// Admin-only management.
	mux.Handle("/admin/users", s.auth.RequireAdmin(http.HandlerFunc(s.handleAdminUsers)))
	mux.Handle("/admin/users/create", s.auth.RequireAdmin(http.HandlerFunc(s.handleAdminUserCreate)))
	mux.Handle("/admin/users/role", s.auth.RequireAdmin(http.HandlerFunc(s.handleAdminUserRole)))
	mux.Handle("/admin/users/status", s.auth.RequireAdmin(http.HandlerFunc(s.handleAdminUserStatus)))
	mux.Handle("/admin/users/reset-password", s.auth.RequireAdmin(http.HandlerFunc(s.handleAdminUserResetPassword)))
	mux.Handle("/admin/slots", s.auth.RequireAdmin(http.HandlerFunc(s.handleAdminSlots)))
	mux.Handle("/admin/envs", s.auth.RequireAdmin(http.HandlerFunc(s.handleAdminEnvs)))
	mux.Handle("/admin/envs/stop", s.auth.RequireAdmin(http.HandlerFunc(s.handleAdminEnvStop)))
	mux.Handle("/admin/envs/start", s.auth.RequireAdmin(http.HandlerFunc(s.handleAdminEnvStart)))
	mux.Handle("/admin/envs/auto", s.auth.RequireAdmin(http.HandlerFunc(s.handleAdminEnvAuto)))
	mux.Handle("/admin/slots/toggle", s.auth.RequireAdmin(http.HandlerFunc(s.handleAdminSlotToggle)))
	mux.Handle("/admin/discovery", s.auth.RequireAdmin(http.HandlerFunc(s.handleDiscoveryPage)))
	mux.Handle("/admin/discovery/run", s.auth.RequireAdmin(http.HandlerFunc(s.handleDiscoveryRun)))
	mux.Handle("/admin/discovery/decide", s.auth.RequireAdmin(http.HandlerFunc(s.handleDiscoveryDecide)))

	mux.Handle("/", s.auth.RequireUser(http.HandlerFunc(s.handleDashboard)))

	// LoadUser wraps everything so handlers can read the current user.
	// recoverPanic is the outermost layer so a panic in any single request
	// never takes the whole server (and thus every other user) down.
	return recoverPanic(s.auth.LoadUser(mux))
}

// recoverPanic turns a panic in any handler into a 500 for that one request
// instead of crashing the process, so one bad request can't take the server
// down for everyone.
func recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic in %s %s: %v", r.Method, r.URL.Path, rec)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type pageData struct {
	Slots           []slotView
	ScannedAt       string
	RefreshSeconds  int
	Error           string
	Username        string
	IsAdmin         bool
	BookingHistory  []store.Reservation
	HistorySlot     string
	HistorySort     string
	HistorySlotOpts []string

	// Automation* describe the staging-automation envs (0pv/0vt/076), listed
	// in config and checked by curl. AutomationEnabled is false when no
	// automation slots are configured, so the section is hidden.
	AutomationEnabled   bool
	AutomationSlots     []slotView
	AutomationScannedAt string
}

// slotView pairs a discovered slot with its current active booking (if any)
// and, when auto power is on, the next automatic stop/start.
type slotView struct {
	discovery.Slot
	Booking *store.Reservation
	Plan    powerPlan
}

// StartStatusLoop checks every known env once synchronously (so the first
// page load has data) and then every discovery.statusSeconds until ctx is
// cancelled. It only curls /actuator/info - finding which envs exist is the
// separate, admin-triggered discovery.
func (s *Server) StartStatusLoop(ctx context.Context) {
	s.refreshStatus(ctx)
	go func() {
		t := time.NewTicker(time.Duration(s.cfg.Discovery.StatusSeconds) * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refreshStatus(ctx)
			}
		}
	}()
}

// refreshStatus re-checks every active env in the inventory and swaps the
// cached result in one go.
func (s *Server) refreshStatus(ctx context.Context) {
	staging, _ := s.st.ListEnvironments(store.ServerStaging)
	automation, _ := s.st.ListEnvironments(store.ServerAutomation)
	stagingSlots := s.disc.StatusStaging(ctx, activeOnly(staging))
	automationSlots := s.disc.StatusAutomation(ctx, s.cfg.Automation, activeOnly(automation))

	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.statusAt = time.Now()
	s.stagingStatus = stagingSlots
	s.automationStatus = automationSlots
	s.settleWarmups(stagingSlots)
	s.settleWarmups(automationSlots)
}

func activeOnly(envs []store.Environment) []store.Environment {
	out := envs[:0:0]
	for _, e := range envs {
		if e.Status == store.EnvActive {
			out = append(out, e)
		}
	}
	return out
}

// cachedStatus returns the last status snapshot.
func (s *Server) cachedStatus() (staging, automation []discovery.Slot, at time.Time) {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	return s.stagingStatus, s.automationStatus, s.statusAt
}

// visible drops slots hidden by config or by the in-app admin setting. The
// filter is applied on every read so show/hide toggles take effect at once.
func (s *Server) visible(slots []discovery.Slot) []discovery.Slot {
	dbHidden, _ := s.st.HiddenSlots()
	out := make([]discovery.Slot, 0, len(slots))
	for _, sl := range slots {
		if s.cfg.Hidden(sl.Name) || dbHidden[sl.Name] {
			continue
		}
		s.applyTransition(&sl)
		out = append(out, sl)
	}
	return out
}

// envNames lists active env names on one server, hidden ones excluded.
func (s *Server) envNames(server string) []string {
	envs, _ := s.st.ListEnvironments(server)
	dbHidden, _ := s.st.HiddenSlots()
	var names []string
	for _, e := range envs {
		if e.Status != store.EnvActive || s.cfg.Hidden(e.Name) || dbHidden[e.Name] {
			continue
		}
		names = append(names, e.Name)
	}
	return names
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	u := auth.UserFrom(r.Context())
	stagingSlots, automationSlots, statusAt := s.cachedStatus()
	slots := s.visible(stagingSlots)

	// Attach the current active booking per slot for the "who's using it" view.
	active, _ := s.st.ListActive()
	current := map[string]*store.Reservation{}
	now := time.Now()
	for i := range active {
		res := active[i]
		if !res.Start.After(now) && res.End.After(now) {
			r := res
			current[res.Slot] = &r
		}
	}
	plans := s.powerPlans(now)
	views := make([]slotView, 0, len(slots))
	for _, sl := range slots {
		views = append(views, slotView{Slot: sl, Booking: current[sl.Name], Plan: plans[sl.Name]})
	}

	// Rolling 7-days-back .. 7-days-forward booking history, with exact
	// times (a booking can be released before its whole-day window ends).
	// Filterable by slot and sortable, same pattern as /deployments.
	historySlot := r.URL.Query().Get("hslot")
	historySort := r.URL.Query().Get("hsort")
	if historySort == "" {
		historySort = "date_desc"
	}
	history, _ := s.st.ListHistory(now.AddDate(0, 0, -7), now.AddDate(0, 0, 7), historySlot, historySort)

	data := pageData{
		Slots:           views,
		ScannedAt:       statusAt.Local().Format("2006-01-02 15:04:05"),
		RefreshSeconds:  s.cfg.Discovery.StatusSeconds,
		BookingHistory:  history,
		HistorySlot:     historySlot,
		HistorySort:     historySort,
		HistorySlotOpts: s.slotNames(),
	}
	if u != nil {
		data.Username = u.Username
		data.IsAdmin = u.IsAdmin()
	}
	autoSlots := s.visible(automationSlots)
	if data.AutomationEnabled = len(autoSlots) > 0; data.AutomationEnabled {
		autoViews := make([]slotView, 0, len(autoSlots))
		for _, sl := range autoSlots {
			autoViews = append(autoViews, slotView{Slot: sl, Booking: current[sl.Name], Plan: plans[sl.Name]})
		}
		data.AutomationSlots = autoViews
		data.AutomationScannedAt = data.ScannedAt
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "dashboard.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	entries, err := s.st.SearchAudit(q, 300)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u := auth.UserFrom(r.Context())
	data := struct {
		Entries  []store.AuditEntry
		Username string
		IsAdmin  bool
		Query    string
	}{Entries: entries, Query: q}
	if u != nil {
		data.Username = u.Username
		data.IsAdmin = true // route is admin-only
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "audit.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleAPISlots(w http.ResponseWriter, r *http.Request) {
	staging, automation, at := s.cachedStatus()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"checkedAt":  at.Format(time.RFC3339),
		"staging":    s.visible(staging),
		"automation": s.visible(automation),
	})
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
