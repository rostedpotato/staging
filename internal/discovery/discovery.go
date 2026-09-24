// Package discovery inspects the staging host (read-only) to build the list of
// slots, their services, versions, and public domains for the dashboard.
package discovery

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
		// shows versions/domains even without container state.
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

		slot := Slot{
			Name:       name,
			Dir:        dir,
			Domains:    domains[name],
			Branch:     branch,
			Commit:     commit,
			Version:    version,
			CommitTime: commitTime,
			Services:   d.servicesFor(name, containers),
		}
		slots = append(slots, slot)
	}

	sort.Slice(slots, func(i, j int) bool { return slots[i].Name < slots[j].Name })
	return slots, nil
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
