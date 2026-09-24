package discovery

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	serverNameRe = regexp.MustCompile(`^\s*server_name\s+([^;]+);`)
	rootRe       = regexp.MustCompile(`^\s*root\s+([^;]+);`)
)

// domainsBySlot scans nginx sites-enabled and maps a slot name to the public
// domains that serve it. A config file is linked to a slot when its `root`
// points at /data/app/agent-<slot>/... . Files without such a root are matched
// heuristically by the slot token appearing in the file name.
func domainsBySlot(sitesDir, appRoot, slotPrefix string, slots []string) map[string][]string {
	out := map[string]map[string]bool{}
	add := func(slot, domain string) {
		domain = strings.TrimSpace(domain)
		if slot == "" || domain == "" || strings.HasPrefix(domain, "~") {
			return
		}
		if out[slot] == nil {
			out[slot] = map[string]bool{}
		}
		out[slot][domain] = true
	}

	entries, err := os.ReadDir(sitesDir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(sitesDir, e.Name())
		names, slotFromRoot := parseSite(path, appRoot, slotPrefix)
		slot := slotFromRoot
		if slot == "" {
			slot = matchSlotByName(e.Name(), slots)
		}
		for _, n := range names {
			add(slot, n)
		}
	}

	res := map[string][]string{}
	for slot, set := range out {
		list := make([]string, 0, len(set))
		for d := range set {
			list = append(list, d)
		}
		sort.Strings(list)
		res[slot] = list
	}
	return res
}

// parseSite returns the server_names and, if a `root` points into
// <appRoot>/<slotPrefix><slot>/..., the slot it belongs to.
func parseSite(path, appRoot, slotPrefix string) (names []string, slot string) {
	f, err := os.Open(path)
	if err != nil {
		return nil, ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if m := serverNameRe.FindStringSubmatch(line); m != nil {
			for _, n := range strings.Fields(m[1]) {
				names = append(names, n)
			}
		}
		if slot == "" {
			if m := rootRe.FindStringSubmatch(line); m != nil {
				slot = slotFromRoot(m[1], appRoot, slotPrefix)
			}
		}
	}
	return names, slot
}

func slotFromRoot(root, appRoot, slotPrefix string) string {
	root = strings.TrimSpace(root)
	base := strings.TrimRight(appRoot, "/") + "/" + slotPrefix
	if !strings.HasPrefix(root, base) {
		return ""
	}
	rest := strings.TrimPrefix(root, base)
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}

// matchSlotByName finds the longest known slot name contained in a filename.
func matchSlotByName(filename string, slots []string) string {
	best := ""
	for _, s := range slots {
		if strings.Contains(filename, s) && len(s) > len(best) {
			best = s
		}
	}
	return best
}
