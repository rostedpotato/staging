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
}

// Overall reports the coarse health of a slot for the dashboard badge.
func (s Slot) Overall() string {
	if len(s.Services) == 0 {
		return "unknown"
	}
	anyDown := false
	anyUnhealthy := false
	for _, svc := range s.Services {
		if !svc.Found || svc.State != "running" {
			anyDown = true
		}
		if svc.Health == "unhealthy" {
			anyUnhealthy = true
		}
	}
	switch {
	case anyDown:
		return "down"
	case anyUnhealthy:
		return "degraded"
	default:
		return "healthy"
	}
}
