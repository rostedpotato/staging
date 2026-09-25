// Package web serves the read-only dashboard (Phase 1).
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

	// rawScanMu guards the cached, unfiltered discovery result. It's kept
	// fresh by a background goroutine (see refreshLoop) rather than by
	// request handlers, so a slow docker/git scan never makes a user wait on
	// a page click. A failed scan keeps the last good result instead of
	// replacing it with empty/broken data - rawErr is only surfaced as a
	// warning banner.
	rawScanMu sync.RWMutex
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

// StartBackgroundScan runs an initial discovery scan synchronously (so the
// first page load has data) and then refreshes it every scanCacheTTL in a
// background goroutine until ctx is cancelled. Request handlers only ever
// read the cached result (see rawScan), so a slow docker/git scan on a busy
// host never blocks a user's click.
func (s *Server) StartBackgroundScan(ctx context.Context) {
	s.refreshScan(ctx)
	go func() {
		t := time.NewTicker(scanCacheTTL)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refreshScan(ctx)
			}
		}
	}()
}

// refreshScan runs discovery and updates the cache. A scan can return slots
// AND an error at once: the slot list comes from disk (always cheap/reliable)
// while the error usually means only `docker ps` failed (container state is
// stale). So we keep any slots we got - even partial data (names, git info,
// domains) beats a blank dashboard - and only fall back to the previous cache
// when this scan produced nothing at all.
func (s *Server) refreshScan(ctx context.Context) {
	slots, err := s.disc.Scan(ctx, nil)
	s.rawScanMu.Lock()
	defer s.rawScanMu.Unlock()
	s.rawErr = err
	s.rawScanAt = time.Now()
	if len(slots) > 0 {
		s.rawSlots = slots
	}
}

// rawScan returns the last cached, unfiltered discovery result.
func (s *Server) rawScan() ([]discovery.Slot, error) {
	s.rawScanMu.RLock()
	defer s.rawScanMu.RUnlock()
	return s.rawSlots, s.rawErr
}

// lastScanAt returns when the cache was last refreshed.
func (s *Server) lastScanAt() time.Time {
	s.rawScanMu.RLock()
	defer s.rawScanMu.RUnlock()
	return s.rawScanAt
}

// scan lists slots, hiding those hidden by config OR by the in-app admin
// setting. The hidden-slot filter itself is always applied fresh (cheap,
// DB-backed) on top of the cached raw scan so admin show/hide toggles are
// reflected immediately.
func (s *Server) scan() ([]discovery.Slot, error) {
	slots, err := s.rawScan()
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
func (s *Server) allSlotNames() []string {
	slots, _ := s.rawScan()
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
	u := auth.UserFrom(r.Context())
	slots, err := s.scan()

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
		ScannedAt:      s.lastScanAt().Local().Format("2006-01-02 15:04:05"),
		RefreshSeconds: s.cfg.Discovery.RefreshSeconds,
	}
	if u != nil {
		data.Username = u.Username
		data.IsAdmin = u.IsAdmin()
	}
	// Only surface a scan error when we have nothing to show. A transient
	// failure (e.g. `docker ps` timing out on a busy host) while cached data
	// is still displayed shouldn't alarm the user - the shown data is fine.
	if err != nil && len(views) == 0 {
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
	slots, err := s.scan()
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusOK) // partial data is still useful
	}
	json.NewEncoder(w).Encode(map[string]any{
		"scannedAt": s.lastScanAt().Format(time.RFC3339),
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
