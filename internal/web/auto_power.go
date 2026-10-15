package web

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"parkee/staging-platform/internal/auth"
	"parkee/staging-platform/internal/store"
)

// autoPowerGrace is both how long an env stays on after its last booking
// ends, and how long before a booking starts a stopped env is switched on.
const autoPowerGrace = 15 * time.Minute

// autoPowerTick is how often the scheduler looks at the bookings.
const autoPowerTick = 30 * time.Second

// autoRetryAfter keeps a failing automatic stop/start from being retried
// every tick.
const autoRetryAfter = 10 * time.Minute

// autoActor is who automatic actions are recorded as in the audit log.
var autoActor = &store.User{Username: "auto-power"}

// powerPlan is the next automatic action for one env.
type powerPlan struct {
	Action string    // "stop" | "start" | "" (nothing planned)
	At     time.Time // when; may be in the past (= due now)
	Reason string
}

// planInput is everything planFor needs about one env.
type planInput struct {
	Desired      string    // store.DesiredRunning | store.DesiredStopped
	DesiredSince time.Time // when Desired last changed (zero = never)
	AutoSince    time.Time // when auto power was last switched on
	Current      *store.Reservation
	Next         *store.Reservation // earliest booking that has not started
	LastEnd      time.Time          // end of the most recent finished booking
}

// planFor decides the next automatic action for one env:
//   - running, not booked: stop 15 min after it became idle. "Idle since" is
//     the latest of: last booking end, auto power switched on, and the env
//     being (re)started, so a manual start or enabling the feature always
//     gives 15 min before anything is stopped. No stop if the next booking
//     would need it started again right away.
//   - stopped: start 15 min before the next booking, or now if a booking is
//     already running (e.g. booked for today while it was stopped).
func planFor(in planInput, now time.Time) powerPlan {
	if in.Desired == store.DesiredStopped {
		switch {
		case in.Current != nil:
			return powerPlan{Action: "start", At: now,
				Reason: fmt.Sprintf("booked by %s now", in.Current.Username)}
		case in.Next != nil:
			return powerPlan{Action: "start", At: in.Next.Start.Add(-autoPowerGrace),
				Reason: fmt.Sprintf("booking by %s starts %s", in.Next.Username, in.Next.Start.Local().Format("2006-01-02 15:04"))}
		}
		return powerPlan{}
	}
	if in.Current != nil {
		return powerPlan{}
	}
	idle := in.LastEnd
	for _, t := range []time.Time{in.AutoSince, in.DesiredSince} {
		if t.After(idle) {
			idle = t
		}
	}
	stopAt := idle.Add(autoPowerGrace)
	if in.Next != nil && !stopAt.Before(in.Next.Start.Add(-autoPowerGrace)) {
		return powerPlan{} // it would have to be started again right away
	}
	reason := "no booking"
	if !in.LastEnd.IsZero() && !in.LastEnd.Before(idle) {
		reason = "last booking ended " + in.LastEnd.Local().Format("2006-01-02 15:04")
	}
	return powerPlan{Action: "stop", At: stopAt, Reason: reason}
}

// powerPlans returns the next automatic action per active env, or nil when
// auto power is off.
func (s *Server) powerPlans(now time.Time) map[string]powerPlan {
	on, since := s.st.AutoPower()
	if !on {
		return nil
	}
	envs, err := s.st.ListEnvironments("")
	if err != nil {
		return nil
	}
	active, _ := s.st.ListActive()
	lastEnds, _ := s.st.LastBookingEnds()
	current := map[string]*store.Reservation{}
	next := map[string]*store.Reservation{}
	for i := range active {
		res := &active[i]
		switch {
		case !res.Start.After(now) && res.End.After(now):
			current[res.Slot] = res
		case res.Start.After(now):
			if old := next[res.Slot]; old == nil || res.Start.Before(old.Start) {
				next[res.Slot] = res
			}
		}
	}
	plans := map[string]powerPlan{}
	for _, e := range envs {
		if e.Status != store.EnvActive {
			continue
		}
		plans[e.Name] = planFor(planInput{
			Desired:      e.DesiredState,
			DesiredSince: e.DesiredChangedAt,
			AutoSince:    since,
			Current:      current[e.Name],
			Next:         next[e.Name],
			LastEnd:      lastEnds[e.Name],
		}, now)
	}
	return plans
}

// StartAutoPowerLoop runs the scheduler every autoPowerTick until ctx ends.
// It does nothing while auto power is switched off.
func (s *Server) StartAutoPowerLoop(ctx context.Context) {
	go func() {
		t := time.NewTicker(autoPowerTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				s.runAutoPower(now)
			}
		}
	}()
}

// runAutoPower starts every automatic action that is due. Actions run in the
// background (same path as the manual buttons), so one slow env never delays
// another.
func (s *Server) runAutoPower(now time.Time) {
	plans := s.powerPlans(now)
	if len(plans) == 0 {
		return
	}
	deploying := map[string]bool{}
	if deps, err := s.st.ActiveDeployments(); err == nil {
		for _, d := range deps {
			deploying[d.Slot] = true
		}
	}
	for name, p := range plans {
		if p.Action == "" || p.At.After(now) || s.recentlyFailed(name, now) {
			continue
		}
		if p.Action == "stop" && deploying[name] {
			continue // try again on a later tick
		}
		env, err := s.st.GetEnvironment(name)
		if err != nil || env == nil {
			continue
		}
		if err := s.startPower(autoActor, env, p.Action, p.Reason); err != nil && err != errPowerBusy {
			log.Printf("auto power %s %s: %v", p.Action, name, err)
		}
	}
}

// recentlyFailed is true if the last stop/start of env failed less than
// autoRetryAfter ago.
func (s *Server) recentlyFailed(name string, now time.Time) bool {
	s.powerMu.Lock()
	defer s.powerMu.Unlock()
	p := s.power[name]
	return p != nil && p.err != "" && now.Sub(p.failedAt) < autoRetryAfter
}

func (s *Server) handleAdminEnvAuto(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/admin/envs", http.StatusSeeOther)
		return
	}
	u := auth.UserFrom(r.Context())
	on := r.FormValue("on") == "1"
	if err := s.st.SetAutoPower(on); err != nil {
		redirectTo(w, r, "/admin/envs", "", err.Error())
		return
	}
	_ = s.st.Audit(u.ID, u.Username, "SET_AUTO_POWER", "setting", store.SettingAutoPower,
		map[string]any{"on": on})
	if on {
		redirectTo(w, r, "/admin/envs", "Automatic stop/start is ON. Idle envs are stopped 15 minutes from now at the earliest.", "")
		return
	}
	redirectTo(w, r, "/admin/envs", "Automatic stop/start is OFF. Envs stay as they are now.", "")
}
