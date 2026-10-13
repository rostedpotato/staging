// Package discovery inspects the staging host (read-only) to build the list of
// slots, their services, versions, and public domains for the dashboard.
package discovery

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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
// info is read from the slot directory on disk, and every service port is
// curled for up/down. It never mutates the host.
func (d *Discoverer) StatusStaging(ctx context.Context, envs []store.Environment) []Slot {
	names := make([]string, 0, len(envs))
	for _, e := range envs {
		names = append(names, e.Name)
	}
	domains := domainsBySlot(d.cfg.NginxSitesDir, d.cfg.AppRoot, d.cfg.SlotDirPrefix, names)

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
			Services:   append(services, wboService(repoDir)),
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
			if svc.HostPort == "" {
				continue
			}
			wg.Add(1)
			go func(svc *Service) {
				defer wg.Done()
				res := probeActuator(ctx, host, svc.HostPort)
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

// wboService reports on WBO (Web Backoffice), which unlike watersheep/
// fisherman isn't a Docker container: it's the slot's frontend built to
// <repoDir>/dist and served directly by nginx. "Found" here means the build
// output exists on disk. The running version comes from dist/details.json
// (written by the frontend build), e.g. {"version": "v1.18.1-b9f5776d", ...} -
// this is WBO's equivalent of the other services' /actuator/info version.
func wboService(repoDir string) Service {
	svc := Service{Name: "wbo", Container: "(static: nginx dist)"}
	distDir := filepath.Join(repoDir, "dist")
	if st, err := os.Stat(filepath.Join(distDir, "index.html")); err == nil && !st.IsDir() {
		svc.Found = true
		svc.State = "running"
		svc.Status = "built, served by nginx"
		svc.RunningVersion = wboDistVersion(distDir)
	} else {
		svc.State = "missing"
		svc.Status = "dist/index.html not found"
	}
	return svc
}

// wboDistVersion reads the "version" field out of dist/details.json.
func wboDistVersion(distDir string) string {
	data, err := os.ReadFile(filepath.Join(distDir, "details.json"))
	if err != nil {
		return ""
	}
	var body struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return ""
	}
	return body.Version
}
