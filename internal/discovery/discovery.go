// Package discovery inspects the staging host (read-only) to build the list of
// slots, their services, versions, and public domains for the dashboard.
package discovery

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"parkee/staging-platform/internal/config"
)

type Discoverer struct {
	cfg config.Discovery
}

func New(cfg config.Discovery) *Discoverer { return &Discoverer{cfg: cfg} }

// Scan returns the current view of all slots. It never mutates the host.
func (d *Discoverer) Scan(ctx context.Context, hidden func(string) bool) ([]Slot, error) {
	slotNames := d.slotDirs()
	domains := domainsBySlot(d.cfg.NginxSitesDir, d.cfg.AppRoot, d.cfg.SlotDirPrefix, slotNames)

	var slots []Slot
	for _, name := range slotNames {
		if hidden != nil && hidden(name) {
			continue
		}
		dir := filepath.Join(d.cfg.AppRoot, d.cfg.SlotDirPrefix+name)
		repoDir := filepath.Join(dir, d.cfg.RepoSubdir)

		branch, commit, commitTime, version := gitInfo(ctx, repoDir)

		services := d.servicesFor(name, dir)
		services = append(services, wboService(repoDir))

		slot := Slot{
			Name:       name,
			Dir:        dir,
			Domains:    domains[name],
			Branch:     branch,
			Commit:     commit,
			Version:    version,
			CommitTime: commitTime,
			Services:   services,
		}
		slots = append(slots, slot)
	}

	sort.Slice(slots, func(i, j int) bool { return slots[i].Name < slots[j].Name })
	d.probeServices(ctx, slots)
	return slots, nil
}

// probeServices curls /actuator/info on every port-backed service across all
// slots concurrently, so the scan costs one probe timeout, not N sequential
// ones. A 2xx answer is "running"; anything else is "down".
func (d *Discoverer) probeServices(ctx context.Context, slots []Slot) {
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
				res := probeActuator(ctx, d.cfg.ProbeHost, svc.HostPort)
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

// slotDirs lists slot names by reading <appRoot>/<slotPrefix>* directories.
// Backup dirs (suffix -bak or containing "_bak") are ignored.
func (d *Discoverer) slotDirs() []string {
	entries, err := os.ReadDir(d.cfg.AppRoot)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), d.cfg.SlotDirPrefix) {
			continue
		}
		name := strings.TrimPrefix(e.Name(), d.cfg.SlotDirPrefix)
		if strings.HasSuffix(name, "-bak") || strings.Contains(name, "_bak") {
			continue
		}
		names = append(names, name)
	}
	return names
}

// servicesFor builds one Service per configured name, with its port read from
// the slot's .env (<SERVICE>_PORT). Services start as "down"; probeServices
// flips them to "running" once /actuator/info answers.
func (d *Discoverer) servicesFor(slot, slotDir string) []Service {
	var out []Service
	for _, svcName := range d.cfg.Services {
		svc := Service{
			Name:      svcName,
			Container: svcName + "-" + slot,
			Found:     true,
			State:     "down",
			Health:    "none",
			HostPort:  slotPort(slotDir, svcName),
		}
		if svc.HostPort == "" {
			svc.Status = "no " + strings.ToUpper(svcName) + "_PORT in .env"
		}
		out = append(out, svc)
	}
	return out
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
