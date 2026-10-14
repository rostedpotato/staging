// Package discovery inspects the staging host (read-only) to build the list of
// slots, their services, versions, and public domains for the dashboard.
package discovery

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"parkee/staging-platform/internal/config"
	"parkee/staging-platform/internal/store"
)

type Discoverer struct {
	cfg config.Discovery
}

func New(cfg config.Discovery) *Discoverer { return &Discoverer{cfg: cfg} }

// StatusStaging builds dashboard cards for the given staging envs: ports come
// from the stored inventory (set by an admin-run discovery), git/nginx/WBO
// info is read from the slot directory on disk, and every service (ports and
// the WBO details.json) is curled for up/down. It never mutates the host.
func (d *Discoverer) StatusStaging(ctx context.Context, envs []store.Environment) []Slot {
	names := make([]string, 0, len(envs))
	for _, e := range envs {
		names = append(names, e.Name)
	}
	domains := domainsBySlot(d.cfg.NginxSitesDir, d.cfg.AppRoot, d.cfg.SlotDirPrefix, names)
	// WBO URLs come only from configs that serve this slot's dist/ folder, so
	// a name match like portainer-ho.conf can't hand "ho" the wrong site.
	wboDomains := wboDomainsBySlot(d.cfg.NginxSitesDir, d.cfg.AppRoot, d.cfg.SlotDirPrefix)

	slots := make([]Slot, 0, len(envs))
	for _, e := range envs {
		dir := filepath.Join(d.cfg.AppRoot, d.cfg.SlotDirPrefix+e.Name)
		repoDir := filepath.Join(dir, d.cfg.RepoSubdir)
		branch, commit, commitTime, version := gitInfo(ctx, repoDir)
		services := d.portServices(e.Ports)
		for i := range services {
			services[i].Container = services[i].Name + "-" + e.Name
		}
		slots = append(slots, Slot{
			Name:       e.Name,
			Dir:        dir,
			Domains:    domains[e.Name],
			Branch:     branch,
			Commit:     commit,
			Version:    version,
			CommitTime: commitTime,
			Services:   append(services, newWBOService(wboURLsFor(wboDomains[e.Name], d.cfg.WBOURL, e.Name))),
		})
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].Name < slots[j].Name })
	probeServices(ctx, d.cfg.ProbeHost, slots)
	return slots
}

// probeServices curls /actuator/info on every port-backed service across all
// slots concurrently, so the scan costs one probe timeout, not N sequential
// ones. A 2xx answer is "running"; anything else is "down".
func probeServices(ctx context.Context, host string, slots []Slot) {
	var wg sync.WaitGroup
	for i := range slots {
		for j := range slots[i].Services {
			svc := &slots[i].Services[j]
			if svc.HostPort == "" && len(svc.wboURLs) == 0 {
				continue
			}
			wg.Add(1)
			go func(svc *Service) {
				defer wg.Done()
				var res probeResult
				if len(svc.wboURLs) > 0 {
					// One URL only: a domain that doesn't answer costs a full
					// probe timeout, so trying several would multiply it.
					res = probeDetails(ctx, svc.wboURLs[0])
				} else {
					res = probeActuator(ctx, host, svc.HostPort)
				}
				svc.Status = res.Status
				if res.Up {
					svc.State, svc.Health = "running", "healthy"
					svc.RunningVersion = res.Version
				}
			}(svc)
		}
	}
	wg.Wait()
}

// newWBOService makes the WBO entry for a slot. WBO is a static frontend
// served by nginx, so it has no port: it's checked by curling its
// details.json (see probeServices).
func newWBOService(urls []string) Service {
	svc := Service{Name: "wbo", Container: "(static: nginx)", Found: true, State: "down", Health: "none", wboURLs: urls}
	if len(urls) == 0 {
		svc.Status = "no WBO url (no nginx domain and no wboUrl template)"
	}
	return svc
}

// wboURLsFor returns the details.json URL to check for an env, from nginx
// domains that serve its dist/ folder: a public domain first
// (sistemparkiran.com preferred), else an internal ".local" name (reached
// through extra_hosts in compose, certificate not verified), else the template
// with {env} filled in. Regex server_names (~...) and ".internal" names are
// never used. Only one URL is returned on purpose, see probeServices.
func wboURLsFor(domains []string, template, env string) []string {
	var public, internal []string
	for _, d := range domains {
		switch {
		case strings.HasPrefix(d, "~"), strings.HasSuffix(d, ".internal"):
		case strings.HasSuffix(d, ".local"):
			internal = append(internal, d)
		default:
			public = append(public, d)
		}
	}
	sort.SliceStable(public, func(i, j int) bool {
		return strings.HasSuffix(public[i], ".sistemparkiran.com") && !strings.HasSuffix(public[j], ".sistemparkiran.com")
	})
	switch {
	case len(public) > 0:
		return []string{"https://" + public[0] + "/details.json"}
	case len(internal) > 0:
		return []string{"https://" + internal[0] + "/details.json"}
	case template != "":
		return []string{strings.ReplaceAll(template, "{env}", env)}
	}
	return nil
}
