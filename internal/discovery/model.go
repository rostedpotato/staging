package discovery

// Service is one deployable component (e.g. watersheep/fisherman) in a slot.
type Service struct {
	Name      string // logical name, e.g. "watersheep"
	Container string // actual container name, e.g. "watersheep-development-09"
	Image     string
	State     string // docker state: running, exited, ...
	Health    string // healthy, unhealthy, starting, none
	Status    string // human status line from docker, e.g. "Up 2 hours"
	HostPort  string // first published host port, if any
	Found     bool   // whether a matching container was found
	// RunningVersion is the build.version reported by the service's own
	// /actuator/info endpoint, i.e. the version actually serving traffic
	// right now (as opposed to Slot.Commit, which is the git checkout).
	// Empty if the service isn't running or doesn't expose the endpoint.
	RunningVersion string

	// wboURLs are the details.json URLs tried (in order) for the WBO service.
	wboURLs []string
}

// Slot is one parallel staging environment (agent-<name>).
type Slot struct {
	Name       string    // e.g. "development-09"
	Dir        string    // e.g. /data/app/agent-development-09
	Domains    []string  // public domains from nginx
	Branch     string    // git branch of the slot's repo
	Commit     string    // short commit hash
	Version    string    // VITE_GIT_FULL_VERSION or tag, if available
	CommitTime string    // author/commit time of HEAD
	Services   []Service // deployable services in this slot

	// Desired is the stored desired state ("running" | "stopped"). An env an
	// admin switched off on purpose is "stopped", which is not a problem.
	Desired string
	// Transition is "stopping" or "starting" while a stop/start is in flight.
	Transition string
}

// IsStopped reports whether the env was stopped on purpose.
func (s Slot) IsStopped() bool { return s.Desired == "stopped" }

// isStatic is true for the WBO entry: nginx serves it, so it keeps working
// while the app containers are stopped.
func (v Service) isStatic() bool { return len(v.wboURLs) > 0 || v.Name == "wbo" }

// markStopped relabels port-backed services that are not answering as
// "stopped" (grey) instead of "down" (red) on an env stopped on purpose.
// The WBO entry is left alone: it is still checked and must stay green.
func (s *Slot) markStopped() {
	if !s.IsStopped() {
		return
	}
	for i := range s.Services {
		svc := &s.Services[i]
		if svc.isStatic() || svc.State == "running" {
			continue
		}
		svc.State, svc.Status = "stopped", "stopped (idle)"
	}
}

// Overall reports the coarse health of a slot for the dashboard badge.
func (s Slot) Overall() string {
	if s.Transition != "" {
		return s.Transition
	}
	if s.IsStopped() {
		appsStopped, staticDown := true, false
		for _, svc := range s.Services {
			switch {
			case svc.isStatic():
				staticDown = staticDown || (svc.State != "running" && svc.State != "unknown")
			case svc.State == "running":
				appsStopped = false
			}
		}
		// Only when every app container is really off. If one is still up
		// (stop half-failed, or Jenkins restarted it) fall through and show
		// the real state instead of hiding it.
		if appsStopped {
			if staticDown {
				return "down"
			}
			return "stopped"
		}
	}
	if len(s.Services) == 0 {
		return "unknown"
	}
	anyDown := false
	anyUnhealthy := false
	checked := 0
	for _, svc := range s.Services {
		if svc.State == "unknown" {
			continue // couldn't be checked, so it can't count as down
		}
		checked++
		if !svc.Found || svc.State != "running" {
			anyDown = true
		}
		if svc.Health == "unhealthy" {
			anyUnhealthy = true
		}
	}
	switch {
	case checked == 0:
		return "unknown"
	case anyDown:
		return "down"
	case anyUnhealthy:
		return "degraded"
	default:
		return "healthy"
	}
}
