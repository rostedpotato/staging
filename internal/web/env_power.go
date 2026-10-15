package web

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"parkee/staging-platform/internal/auth"
	"parkee/staging-platform/internal/discovery"
	"parkee/staging-platform/internal/store"
)

// warmupWindow is how long a freshly started env may show "starting" while
// watersheep boots (a few minutes) before a non-answering service counts as
// a real problem again.
const warmupWindow = 10 * time.Minute

// powerState tracks one in-flight (or just finished) stop/start of an env.
type powerState struct {
	kind  string    // "stopping" | "starting"
	until time.Time // zero while the compose command is still running
	err   string    // last failure, shown on /admin/envs
}

// envRow is one line of /admin/envs.
type envRow struct {
	Name       string
	Server     string
	Overall    string // healthy | degraded | down | stopped | stopping | starting | unknown
	Desired    string
	BookedBy   string // current booking, if any
	BookedTill time.Time
	NextBook   string // upcoming booking summary, if any
	Busy       bool
	Error      string
}

type envsPage struct {
	Username string
	IsAdmin  bool
	Notice   string
	Error    string
	Rows     []envRow
}

func (s *Server) handleAdminEnvs(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	staging, automation, _ := s.cachedStatus()
	live := map[string]discovery.Slot{}
	for _, sl := range append(append([]discovery.Slot{}, staging...), automation...) {
		live[sl.Name] = sl
	}
	active, _ := s.st.ListActive()
	now := time.Now()
	current := map[string]store.Reservation{}
	upcoming := map[string]store.Reservation{}
	for _, res := range active {
		switch {
		case !res.Start.After(now) && res.End.After(now):
			current[res.Slot] = res
		case res.Start.After(now):
			if old, ok := upcoming[res.Slot]; !ok || res.Start.Before(old.Start) {
				upcoming[res.Slot] = res
			}
		}
	}

	envs, _ := s.st.ListEnvironments("")
	var rows []envRow
	for _, e := range envs {
		if e.Status != store.EnvActive {
			continue
		}
		sl := live[e.Name]
		s.applyTransition(&sl)
		row := envRow{Name: e.Name, Server: e.Server, Desired: e.DesiredState, Overall: sl.Overall()}
		if sl.Name == "" {
			row.Overall = "unknown" // not checked yet
		}
		if res, ok := current[e.Name]; ok {
			row.BookedBy, row.BookedTill = res.Username, res.End
		}
		if res, ok := upcoming[e.Name]; ok {
			row.NextBook = fmt.Sprintf("%s from %s", res.Username, res.Start.Local().Format("2006-01-02"))
		}
		s.powerMu.Lock()
		if p := s.power[e.Name]; p != nil {
			row.Error = p.err
			row.Busy = p.kind == "stopping" || (p.kind == "starting" && p.until.IsZero())
		}
		s.powerMu.Unlock()
		rows = append(rows, row)
	}
	s.render(w, "admin_envs.html", envsPage{
		Username: u.Username,
		IsAdmin:  true,
		Notice:   r.URL.Query().Get("notice"),
		Error:    r.URL.Query().Get("error"),
		Rows:     rows,
	})
}

func (s *Server) handleAdminEnvStop(w http.ResponseWriter, r *http.Request) {
	s.handleEnvPower(w, r, "stop")
}
func (s *Server) handleAdminEnvStart(w http.ResponseWriter, r *http.Request) {
	s.handleEnvPower(w, r, "start")
}

func (s *Server) handleEnvPower(w http.ResponseWriter, r *http.Request, verb string) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/admin/envs", http.StatusSeeOther)
		return
	}
	u := auth.UserFrom(r.Context())
	name := r.FormValue("env")
	env, err := s.st.GetEnvironment(name)
	if err != nil || env == nil || env.Status != store.EnvActive {
		redirectTo(w, r, "/admin/envs", "", "unknown env "+name)
		return
	}

	notice := ""
	if verb == "stop" {
		// Never switch off an env somebody is using.
		if res, _ := s.st.ActiveForSlot(name); res != nil {
			redirectTo(w, r, "/admin/envs", "",
				fmt.Sprintf("%s is booked by %s until %s; it is not stopped.", name, res.Username, res.End.Local().Format("2006-01-02 15:04")))
			return
		}
		// A Jenkins deploy started from this platform is running there.
		if deps, _ := s.st.ActiveDeployments(); len(deps) > 0 {
			for _, d := range deps {
				if d.Slot == name {
					redirectTo(w, r, "/admin/envs", "", fmt.Sprintf("a deployment (#%d) is still running on %s; it is not stopped.", d.ID, name))
					return
				}
			}
		}
		notice = fmt.Sprintf("Stopping %s in the background.", name)
		if next := s.nextBooking(name); next != "" {
			notice += " Note: " + next + " Start it again before then; nothing starts it automatically yet."
		}
	} else {
		notice = fmt.Sprintf("Starting %s in the background. watersheep can take a few minutes to answer.", name)
	}

	if !s.beginPower(name, verb) {
		redirectTo(w, r, "/admin/envs", "", name+" is already being stopped or started.")
		return
	}
	// Mark the intent before touching containers, so a deploy cannot slip in
	// between. Start flips back to "running" only once compose succeeded.
	if verb == "stop" {
		if err := s.st.SetDesiredState(name, store.DesiredStopped); err != nil {
			s.endPower(name, "", err)
			redirectTo(w, r, "/admin/envs", "", err.Error())
			return
		}
	}
	_ = s.st.Audit(u.ID, u.Username, "REQUEST_"+upper(verb)+"_ENV", "env", name,
		map[string]any{"server": env.Server})
	go s.runPower(u, env.Server, name, verb)
	redirectTo(w, r, "/admin/envs", notice, "")
}

// nextBooking describes the earliest upcoming booking of an env, or "".
func (s *Server) nextBooking(env string) string {
	active, _ := s.st.ListActive()
	now := time.Now()
	var best *store.Reservation
	for i := range active {
		res := active[i]
		if res.Slot == env && res.Start.After(now) && (best == nil || res.Start.Before(best.Start)) {
			best = &res
		}
	}
	if best == nil {
		return ""
	}
	return fmt.Sprintf("%s has a booking by %s from %s.", env, best.Username, best.Start.Local().Format("2006-01-02"))
}

// runPower does the actual compose stop/start. It runs in the background:
// a stop can take a minute because JVMs that ignore SIGTERM are killed only
// after docker's grace period.
func (s *Server) runPower(u *store.User, server, name, verb string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var err error
	if verb == "stop" {
		err = s.disc.StopEnv(ctx, server, name, s.cfg.Automation)
	} else {
		err = s.disc.StartEnv(ctx, server, name, s.cfg.Automation)
		if err == nil {
			err = s.st.SetDesiredState(name, store.DesiredRunning)
		}
	}
	meta := map[string]any{"server": server}
	if err != nil {
		log.Printf("env %s %s: %v", verb, name, err)
		meta["error"] = err.Error()
		_ = s.st.Audit(u.ID, u.Username, upper(verb)+"_ENV_FAILED", "env", name, meta)
	} else {
		_ = s.st.Audit(u.ID, u.Username, upper(verb)+"_ENV", "env", name, meta)
	}
	s.endPower(name, verb, err)
	s.refreshStatus(context.Background())
}

// beginPower claims an env for one stop/start at a time.
func (s *Server) beginPower(name, verb string) bool {
	s.powerMu.Lock()
	defer s.powerMu.Unlock()
	if p := s.power[name]; p != nil && (p.kind == "stopping" || (p.kind == "starting" && p.until.IsZero())) {
		return false
	}
	kind := "stopping"
	if verb == "start" {
		kind = "starting"
	}
	s.power[name] = &powerState{kind: kind}
	return true
}

// endPower finishes a stop/start. After a successful start the env stays
// "starting" until it answers or warmupWindow passes; every other outcome
// drops the transition (a failure keeps its message for /admin/envs).
func (s *Server) endPower(name, verb string, err error) {
	s.powerMu.Lock()
	defer s.powerMu.Unlock()
	switch {
	case err != nil:
		s.power[name] = &powerState{err: err.Error()}
	case verb == "start":
		s.power[name] = &powerState{kind: "starting", until: time.Now().Add(warmupWindow)}
	default:
		delete(s.power, name)
	}
}

// applyTransition overlays an in-flight stop/start on a status snapshot copy.
func (s *Server) applyTransition(sl *discovery.Slot) {
	s.powerMu.Lock()
	defer s.powerMu.Unlock()
	if p := s.power[sl.Name]; p != nil {
		sl.Transition = p.kind
	}
}

// settleWarmups ends "starting" for envs whose app services all answer, or
// whose warmup window has passed.
func (s *Server) settleWarmups(slots []discovery.Slot) {
	s.powerMu.Lock()
	defer s.powerMu.Unlock()
	for _, sl := range slots {
		p := s.power[sl.Name]
		if p == nil || p.kind != "starting" || p.until.IsZero() {
			continue
		}
		if time.Now().After(p.until) || appsUp(sl) {
			delete(s.power, sl.Name)
		}
	}
}

// appsUp is true when every port-backed service of the slot answers.
func appsUp(sl discovery.Slot) bool {
	n := 0
	for _, svc := range sl.Services {
		if svc.HostPort == "" {
			continue
		}
		n++
		if svc.State != "running" {
			return false
		}
	}
	return n > 0
}

func upper(verb string) string {
	if verb == "stop" {
		return "STOP"
	}
	return "START"
}
