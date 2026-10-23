package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"parkee/staging-platform/internal/auth"
	"parkee/staging-platform/internal/discovery"
	"parkee/staging-platform/internal/store"
)

// warmupWindow is the longest a freshly started env shows "starting" while
// watersheep boots. After it, a non-answering service counts as a real
// problem again. ("starting" normally ends sooner: as soon as both app
// services answer.)
const warmupWindow = 10 * time.Minute

// powerState tracks one in-flight (or just finished) stop/start of an env.
type powerState struct {
	kind     string    // "stopping" | "starting"
	until    time.Time // zero while the compose command is still running
	err      string    // last failure, shown on /admin/envs
	failedAt time.Time // when err happened (auto power waits before retrying)
}

// errPowerBusy means a stop/start of that env is already running.
var errPowerBusy = errors.New("already being stopped or started")

// errEnvBooked means a stop was refused because somebody holds the env now.
var errEnvBooked = errors.New("the env is booked right now")

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
	Plan       powerPlan // next automatic action (empty when auto power is off)
}

type envsPage struct {
	Username  string
	IsAdmin   bool
	Notice    string
	Error     string
	Rows      []envRow
	AutoPower bool
	AutoSince time.Time
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

	plans := s.powerPlans(now)
	envs, _ := s.st.ListEnvironments("")
	var rows []envRow
	for _, e := range envs {
		if e.Status != store.EnvActive {
			continue
		}
		sl := live[e.Name]
		s.applyTransition(&sl)
		row := envRow{Name: e.Name, Server: e.Server, Desired: e.DesiredState, Overall: sl.Overall(), Plan: plans[e.Name]}
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
	autoOn, autoSince := s.st.AutoPower()
	s.render(w, "admin_envs.html", envsPage{
		Username:  u.Username,
		IsAdmin:   true,
		Notice:    r.URL.Query().Get("notice"),
		Error:     r.URL.Query().Get("error"),
		Rows:      rows,
		AutoPower: autoOn,
		AutoSince: autoSince,
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
		notice = fmt.Sprintf("Stopping %s in the background.", name)
		if next := s.nextBooking(name); next != "" {
			if on, _ := s.st.AutoPower(); on {
				notice += " " + next + " It is started automatically 15 minutes before."
			} else {
				notice += " Note: " + next + " Automatic stop/start is off: start it again before then."
			}
		}
	} else {
		notice = fmt.Sprintf("Starting %s in the background. watersheep needs a moment before it answers.", name)
	}

	if err := s.startPower(u, env, verb, "manual"); err != nil {
		if err == errPowerBusy {
			redirectTo(w, r, "/admin/envs", "", name+" is already being stopped or started.")
			return
		}
		redirectTo(w, r, "/admin/envs", "", err.Error())
		return
	}
	redirectTo(w, r, "/admin/envs", notice, "")
}

// startPower claims the env, records the intent and runs compose in the
// background. Used by the admin buttons and by the auto power scheduler, so
// both go through the same locking, desired state and audit trail.
func (s *Server) startPower(u *store.User, env *store.Environment, verb, reason string) error {
	if !s.beginPower(env.Name, verb) {
		return errPowerBusy
	}
	// Last look at the bookings, as late as possible: a plan made a moment ago
	// (or a button page opened a while ago) may be out of date. If a booking
	// slips in after this check, auto power starts the env again on its next
	// tick (a stopped env with a running booking is started at once).
	if verb == "stop" {
		if res, _ := s.st.ActiveForSlot(env.Name); res != nil {
			s.abortPower(env.Name)
			return errEnvBooked
		}
	}
	// Mark the intent before touching containers, then look for a running
	// deployment. deploy.Start does the opposite (takes the slot lock, then
	// reads the intent), so a stop and a deploy can never both go ahead.
	// Start flips back to "running" only once compose succeeded.
	if verb == "stop" {
		if err := s.st.SetDesiredState(env.Name, store.DesiredStopped); err != nil {
			s.endPower(env.Name, "", err)
			return err
		}
		if d := s.activeDeployment(env.Name); d != nil {
			_ = s.st.SetDesiredState(env.Name, store.DesiredRunning)
			s.abortPower(env.Name)
			return fmt.Errorf("a deployment (#%d) is still running on %s; it is not stopped", d.ID, env.Name)
		}
	}
	_ = s.st.Audit(u.ID, u.Username, "REQUEST_"+upper(verb)+"_ENV", "env", env.Name,
		map[string]any{"server": env.Server, "reason": reason})
	go s.runPower(u, env.Server, env.Name, verb)
	return nil
}

// activeDeployment returns a queued/running platform deployment on env, or nil.
func (s *Server) activeDeployment(env string) *store.Deployment {
	deps, _ := s.st.ActiveDeployments()
	for i := range deps {
		if deps[i].Slot == env {
			return &deps[i]
		}
	}
	return nil
}

// abortPower releases an env claimed by beginPower without recording an error.
func (s *Server) abortPower(name string) {
	s.powerMu.Lock()
	defer s.powerMu.Unlock()
	delete(s.power, name)
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
		if err != nil {
			// The env may be fully or partly up. Leaving it marked "stopped"
			// would block deploys and hide it from auto power, so go back to
			// "running": the dashboard then shows its real state.
			_ = s.st.SetDesiredState(name, store.DesiredRunning)
		}
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

// isBusy reports whether a stop/start of env is still running.
func (s *Server) isBusy(name string) bool {
	s.powerMu.Lock()
	defer s.powerMu.Unlock()
	p := s.power[name]
	return p != nil && (p.kind == "stopping" || (p.kind == "starting" && p.until.IsZero()))
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
		s.power[name] = &powerState{err: err.Error(), failedAt: time.Now()}
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
