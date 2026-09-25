// Package web serves the read-only dashboard (Phase 1).
package web

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
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

// scanCacheTTL bounds how often the (relatively expensive: docker ps + git
// subprocesses per slot) discovery scan actually runs. Dashboard freshness
// isn't critical here (this isn't a monitoring tool), so a coarse cache
// keeps repeated page loads/clicks cheap.
const scanCacheTTL = time.Minute

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

	// rawScanMu guards the cached, unfiltered discovery result. Hidden-slot
	// filtering is applied fresh on every call (cheap, DB-only) so admin
	// show/hide toggles take effect immediately despite the scan cache.
	rawScanMu sync.Mutex
	rawScanAt time.Time
	rawSlots  []discovery.Slot
	rawErr    error
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
	return &Server{cfg: cfg, disc: disc, auth: a, st: st, deploy: dep, tmpl: tmpl}, nil
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
	mux.Handle("/admin/slots/toggle", s.auth.RequireAdmin(http.HandlerFunc(s.handleAdminSlotToggle)))

	mux.Handle("/", s.auth.RequireUser(http.HandlerFunc(s.handleDashboard)))

	// LoadUser wraps everything so handlers can read the current user.
	return s.auth.LoadUser(mux)
}

type pageData struct {
	Slots          []slotView
	ScannedAt      string
	RefreshSeconds int
	Error          string
	Username       string
	IsAdmin        bool
}

// slotView pairs a discovered slot with its current active booking (if any).
type slotView struct {
	discovery.Slot
	Booking *store.Reservation
}

// rawScan runs (or returns the cached result of) an unfiltered discovery
// scan. Cached for scanCacheTTL since discovery shells out to docker/git for
// every slot on every call, which is too costly to redo on each
// request/click; this isn't a monitoring tool so slightly stale
// versions/container state is fine.
func (s *Server) rawScan(ctx context.Context) ([]discovery.Slot, error) {
	s.rawScanMu.Lock()
	defer s.rawScanMu.Unlock()
	if time.Since(s.rawScanAt) < scanCacheTTL {
		return s.rawSlots, s.rawErr
	}
	s.rawSlots, s.rawErr = s.disc.Scan(ctx, nil)
	s.rawScanAt = time.Now()
	return s.rawSlots, s.rawErr
}

// scan lists slots, hiding those hidden by config OR by the in-app admin
// setting. The hidden-slot filter itself is always applied fresh (cheap,
// DB-backed) on top of the cached raw scan so admin show/hide toggles are
// reflected immediately.
func (s *Server) scan(ctx context.Context) ([]discovery.Slot, error) {
	slots, err := s.rawScan(ctx)
	dbHidden, _ := s.st.HiddenSlots()
	visible := make([]discovery.Slot, 0, len(slots))
	for _, sl := range slots {
		if s.cfg.Hidden(sl.Name) || dbHidden[sl.Name] {
			continue
		}
		visible = append(visible, sl)
	}
	return visible, err
}

// allSlotNames returns every discovered slot (including hidden), for admin UI.
func (s *Server) allSlotNames(ctx context.Context) []string {
	slots, _ := s.rawScan(ctx)
	names := make([]string, 0, len(slots))
	for _, sl := range slots {
		names = append(names, sl.Name)
	}
	return names
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()

	u := auth.UserFrom(r.Context())
	slots, err := s.scan(ctx)

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
	views := make([]slotView, 0, len(slots))
	for _, sl := range slots {
		views = append(views, slotView{Slot: sl, Booking: current[sl.Name]})
	}

	data := pageData{
		Slots:          views,
		ScannedAt:      time.Now().Format("2006-01-02 15:04:05"),
		RefreshSeconds: s.cfg.Discovery.RefreshSeconds,
	}
	if u != nil {
		data.Username = u.Username
		data.IsAdmin = u.IsAdmin()
	}
	if err != nil {
		data.Error = err.Error()
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
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()

	slots, err := s.scan(ctx)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusOK) // partial data is still useful
	}
	json.NewEncoder(w).Encode(map[string]any{
		"scannedAt": time.Now().Format(time.RFC3339),
		"slots":     slots,
		"error":     errStr(err),
	})
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
