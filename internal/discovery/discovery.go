// Package discovery inspects the staging host (read-only) to build the list of
// slots, their services, versions, and public domains for the dashboard.
package discovery

import (
	"context"
	"encoding/json"
	"log"
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

	containers, err := d.listContainers(ctx)
	if err != nil {
		// Docker may be unreachable; still return slots from disk so the UI
		// shows versions/domains even without container state. Log it though,
		// since a silent failure here makes every service look "down".
		log.Printf("discovery: docker ps failed, services will show as not found: %v", err)
		containers = nil
	}

	var slots []Slot
	for _, name := range slotNames {
		if hidden != nil && hidden(name) {
			continue
		}
		dir := filepath.Join(d.cfg.AppRoot, d.cfg.SlotDirPrefix+name)
		repoDir := filepath.Join(dir, d.cfg.RepoSubdir)

		branch, commit, commitTime, version := gitInfo(ctx, repoDir)

		services := d.servicesFor(name, containers)
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
	fetchRunningVersions(ctx, slots)
	return slots, err
}

// fetchRunningVersions hits /actuator/info on every running service across
// all slots, concurrently, so this doesn't add up to N*services sequential
// HTTP round-trips on a scan that already runs in the background.
func fetchRunningVersions(ctx context.Context, slots []Slot) {
	var wg sync.WaitGroup
	for i := range slots {
		for j := range slots[i].Services {
			svc := &slots[i].Services[j]
			if !svc.Found || svc.HostPort == "" {
				continue
			}
			wg.Add(1)
			go func(svc *Service) {
				defer wg.Done()
				svc.RunningVersion = runtimeVersion(ctx, svc.HostPort)
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

// servicesFor matches configured service names to containers named
// "<service>-<slot>".
func (d *Discoverer) servicesFor(slot string, containers []dockerContainer) []Service {
	byName := map[string]dockerContainer{}
	for _, c := range containers {
		byName[c.Names] = c
	}
	var out []Service
	for _, svcName := range d.cfg.Services {
		want := svcName + "-" + slot
		svc := Service{Name: svcName, Container: want}
		if c, ok := byName[want]; ok {
			svc.Found = true
			svc.Image = c.Image
			svc.State = strings.ToLower(c.State)
			svc.Status = c.Status
			svc.Health = health(c.Status)
			svc.HostPort = firstHostPort(c.Ports)
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
