package discovery

import (
	"context"
	"net/url"
	"sort"
	"strconv"

	"parkee/staging-platform/internal/config"
	"parkee/staging-platform/internal/store"
)

// StatusAutomation builds dashboard cards for automation envs from the stored
// inventory: /actuator/info on each port at auto.Host, and WBO via its
// details.json URL (auto.WBOURL). There is no git or nginx info for that
// server, so cards only carry status, versions and the WBO domain.
func (d *Discoverer) StatusAutomation(ctx context.Context, auto config.Automation, envs []store.Environment) []Slot {
	slots := make([]Slot, 0, len(envs))
	for _, e := range envs {
		urls := wboURLsFor(nil, auto.WBOURL, e.Name)
		slot := Slot{Name: e.Name, Services: append(d.portServices(e.Ports), newWBOService(urls))}
		for _, raw := range urls {
			if u, err := url.Parse(raw); err == nil && u.Host != "" {
				slot.Domains = append(slot.Domains, u.Host)
			}
		}
		slots = append(slots, slot)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].Name < slots[j].Name })
	probeServices(ctx, auto.Host, slots)
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
