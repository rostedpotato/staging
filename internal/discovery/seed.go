package discovery

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"parkee/staging-platform/internal/config"
	"parkee/staging-platform/internal/store"
)

// SeedEnvs builds a first inventory without running docker, so the dashboard
// isn't empty before an admin has run discovery: staging envs come from
// <appRoot>/<prefix>*/.env (<SERVICE>_PORT), automation envs from config.
// It's only used when the environments table is empty.
func (d *Discoverer) SeedEnvs(auto config.Automation) []store.EnvChange {
	var out []store.EnvChange
	entries, _ := os.ReadDir(d.cfg.AppRoot)
	for _, e := range entries {
		name, ok := strings.CutPrefix(e.Name(), d.cfg.SlotDirPrefix)
		if !e.IsDir() || !ok || !envNameRe.MatchString(name) ||
			strings.HasSuffix(name, "-bak") || strings.Contains(name, "_bak") {
			continue
		}
		ports := map[string]int{}
		envFile := filepath.Join(d.cfg.AppRoot, e.Name(), ".env")
		for _, svc := range d.cfg.Services {
			if p, err := strconv.Atoi(envValue(envFile, strings.ToUpper(svc)+"_PORT")); err == nil && p > 0 {
				ports[svc] = p
			}
		}
		out = append(out, store.EnvChange{Name: name, Server: store.ServerStaging, Kind: "new", Ports: ports})
	}
	for _, s := range auto.Slots {
		out = append(out, store.EnvChange{Name: s.Name, Server: store.ServerAutomation, Kind: "new", Ports: s.Ports})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// envValue returns the value of KEY in a dotenv-style file, or "".
func envValue(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}
