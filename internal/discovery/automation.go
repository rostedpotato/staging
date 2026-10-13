package discovery

import (
	"context"
	"sort"
	"strconv"

	"parkee/staging-platform/internal/store"
)

// StatusAutomation builds dashboard cards for automation envs from the stored
// inventory and curls /actuator/info on each port at host. There is no git or
// nginx info for that server, so cards only carry status and versions.
func (d *Discoverer) StatusAutomation(ctx context.Context, host string, envs []store.Environment) []Slot {
	slots := make([]Slot, 0, len(envs))
	for _, e := range envs {
		slots = append(slots, Slot{Name: e.Name, Services: d.portServices(e.Ports)})
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].Name < slots[j].Name })
	probeServices(ctx, host, slots)
	return slots
}

// portServices makes one down-by-default Service per configured service name,
// using the port recorded for it by discovery.
func (d *Discoverer) portServices(ports map[string]int) []Service {
	var out []Service
	for _, name := range d.cfg.Services {
		svc := Service{Name: name, Found: true, State: "down", Health: "none"}
		if p := ports[name]; p > 0 {
			svc.HostPort = strconv.Itoa(p)
		} else {
			svc.Status = "no port found by discovery"
		}
		out = append(out, svc)
	}
	return out
}
