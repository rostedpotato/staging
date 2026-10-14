package discovery

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"parkee/staging-platform/internal/config"
	"parkee/staging-platform/internal/store"
)

func portNum(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

func upServer(t *testing.T, version string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/actuator/info":
			w.Write([]byte(`{"build":{"version":"` + version + `"}}`))
		case "/details.json":
			w.Write([]byte(`{"version":"v1.18.0-44924546","location_name":"unknown"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// closedPort returns a port that was just released: connection refused.
func closedPort(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	p := portNum(t, srv)
	srv.Close()
	return p
}

func TestStatusStagingProbesStoredPorts(t *testing.T) {
	up := upServer(t, "v3.10.0-abc")
	d := New(config.Discovery{
		AppRoot: t.TempDir(), SlotDirPrefix: "agent-", RepoSubdir: "parkee-agent-backoffice",
		NginxSitesDir: t.TempDir(), Services: []string{"watersheep", "fisherman"},
		ProbeHost: "127.0.0.1",
	})
	slots := d.StatusStaging(context.Background(), []store.Environment{
		{Name: "dev-01", Ports: map[string]int{"watersheep": portNum(t, up), "fisherman": closedPort(t)}},
	})
	if len(slots) != 1 {
		t.Fatalf("want 1 slot, got %d", len(slots))
	}
	byName := map[string]Service{}
	for _, s := range slots[0].Services {
		byName[s.Name] = s
	}
	if ws := byName["watersheep"]; ws.State != "running" || ws.RunningVersion != "v3.10.0-abc" {
		t.Fatalf("watersheep should be running with version, got %+v", ws)
	}
	if fs := byName["fisherman"]; fs.State != "down" || fs.Status != "no response: connection refused" {
		t.Fatalf("fisherman should be down/refused, got %+v", fs)
	}
	if got := slots[0].Overall(); got != "down" {
		t.Fatalf("one dead service must make the slot down, got %q", got)
	}
}

func TestStatusAutomationMissingPort(t *testing.T) {
	up := upServer(t, "v2.95.4-988a036d9c")
	d := New(config.Discovery{Services: []string{"watersheep", "fisherman"}})
	slots := d.StatusAutomation(context.Background(), config.Automation{Host: "127.0.0.1", WBOURL: up.URL + "/details.json"}, []store.Environment{
		{Name: "0vt", Ports: map[string]int{"watersheep": portNum(t, up), "fisherman": portNum(t, up)}},
		{Name: "0pv", Ports: map[string]int{"watersheep": portNum(t, up)}},
	})
	if got := slots[1].Overall(); got != "healthy" { // sorted: 0pv, 0vt
		t.Fatalf("0vt should be healthy, got %q", got)
	}
	if got := slots[0].Overall(); got != "down" {
		t.Fatalf("0pv lacks a fisherman port, want down, got %q", got)
	}
	if s := slots[0].Services[1].Status; s != "no port found by discovery" {
		t.Fatalf("missing port status = %q", s)
	}
	// WBO is checked through details.json and reports its version.
	var wbo Service
	for _, svc := range slots[1].Services {
		if svc.Name == "wbo" {
			wbo = svc
		}
	}
	if wbo.State != "running" || wbo.RunningVersion != "v1.18.0-44924546" {
		t.Fatalf("wbo should be running with version, got %+v", wbo)
	}
}

// Real `docker ps --format '{{.Names}}|{{.Ports}}'` lines from both servers.
const stagingPs = `staging-platform|0.0.0.0:8088->8088/tcp, :::8088->8088/tcp
watersheep-development-04|0.0.0.0:9011->9005/tcp, :::9011->9005/tcp
fisherman-development-04|0.0.0.0:4011->8888/tcp, :::4011->8888/tcp
kafka-development-04|0.0.0.0:10011->9092/tcp, :::10011->9092/tcp
migration-service-development-09|
watersheep-06r|0.0.0.0:9007->9005/tcp, :::9007->9005/tcp
fisherman-06r|0.0.0.0:4007->8888/tcp, :::4007->8888/tcp
kafka-06r|10.20.1.12:10007->9092/tcp
fisherman-development-10|0.0.0.0:4021->8888/tcp, :::4021->8888/tcp
watersheep-development-10|0.0.0.0:9021->9005/tcp, :::9021->9005/tcp
watersheep-ho|`

const automationPs = `watersheep-automation-0vt|0.0.0.0:9004->9005/tcp, :::9004->9005/tcp
fisherman-automation-0vt|0.0.0.0:4004->8888/tcp, :::4004->8888/tcp
kafka-automation-0vt|0.0.0.0:10004->9092/tcp, :::10004->9092/tcp
watersheep-automation-0pv|0.0.0.0:9000->9005/tcp, :::9000->9005/tcp
fisherman-automation-0pv|0.0.0.0:4000->8888/tcp, :::4000->8888/tcp
postgres-automation-0pv|0.0.0.0:5000->5432/tcp, :::5000->5432/tcp`

func envMap(envs []store.EnvChange) map[string]map[string]int {
	m := map[string]map[string]int{}
	for _, e := range envs {
		m[e.Name] = e.Ports
	}
	return m
}

func TestParseDockerPsStaging(t *testing.T) {
	got := envMap(parseDockerPs(stagingPs, store.ServerStaging, "-", []string{"watersheep", "fisherman"}))
	want := map[string]map[string]int{
		"development-04": {"watersheep": 9011, "fisherman": 4011},
		"06r":            {"watersheep": 9007, "fisherman": 4007},
		"development-10": {"watersheep": 9021, "fisherman": 4021},
		"ho":             {}, // stopped container: env exists, no published port
	}
	if len(got) != len(want) {
		t.Fatalf("envs = %v, want %v", got, want)
	}
	for name, ports := range want {
		if !samePortMap(got[name], ports) {
			t.Fatalf("%s ports = %v, want %v", name, got[name], ports)
		}
	}
}

func TestParseDockerPsAutomation(t *testing.T) {
	got := envMap(parseDockerPs(automationPs, store.ServerAutomation, "-automation-", []string{"watersheep", "fisherman"}))
	if len(got) != 2 || got["0vt"]["watersheep"] != 9004 || got["0pv"]["fisherman"] != 4000 {
		t.Fatalf("automation envs = %v", got)
	}
}

func samePortMap(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestDiscoverAutomationOverSSH swaps the ssh binary for a script that checks
// the arguments it gets and prints canned docker ps output.
func TestDiscoverAutomationOverSSH(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	fake := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\ncat <<'EOF'\n" + automationPs + "\nEOF\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	d := New(config.Discovery{Services: []string{"watersheep", "fisherman"}})
	envs, err := d.DiscoverAutomation(context.Background(), config.Automation{
		Host: "10.20.3.142", ContainerInfix: "automation-",
		SSH: config.AutomationSSH{User: "parkee", Port: 22, Bin: fake, KnownHostsPath: filepath.Join(dir, "kh")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 2 || envs[0].Server != store.ServerAutomation {
		t.Fatalf("envs = %+v", envs)
	}
	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{"BatchMode=yes", "parkee@10.20.3.142", "docker ps -a --format '{{.Names}}|{{.Ports}}'"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("ssh args missing %q:\n%s", want, args)
		}
	}
}

func TestDiscoverAutomationSSHFailure(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\necho 'parkee@10.20.3.142: Permission denied (publickey).' >&2\nexit 255\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	d := New(config.Discovery{Services: []string{"watersheep"}})
	_, err := d.DiscoverAutomation(context.Background(), config.Automation{
		Host: "10.20.3.142", ContainerInfix: "automation-",
		SSH: config.AutomationSSH{User: "parkee", Port: 22, Bin: fake},
	})
	if err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("want ssh stderr in error, got %v", err)
	}
}

func TestDiffEnvironments(t *testing.T) {
	stored := []store.Environment{
		{Name: "ho", Server: store.ServerStaging, Ports: map[string]int{"watersheep": 9000}, Status: store.EnvActive},
		{Name: "06r", Server: store.ServerStaging, Ports: map[string]int{"watersheep": 9007}, Status: store.EnvActive},
		{Name: "dev-old", Server: store.ServerStaging, Status: store.EnvActive},
		{Name: "0vt", Server: store.ServerAutomation, Ports: map[string]int{"watersheep": 9004}, Status: store.EnvActive},
	}
	found := []store.EnvChange{
		{Name: "ho", Server: store.ServerStaging, Ports: map[string]int{"watersheep": 9000}},
		{Name: "06r", Server: store.ServerStaging, Ports: map[string]int{"watersheep": 9017}},
		{Name: "dev-new", Server: store.ServerStaging, Ports: map[string]int{"watersheep": 9030}},
	}
	// Automation was not reachable: 0vt must NOT be reported missing.
	diff := store.DiffEnvironments(stored, found, map[string]bool{store.ServerStaging: true})
	kinds := map[string]string{}
	for _, c := range diff {
		kinds[c.Name] = c.Kind
	}
	want := map[string]string{"ho": "unchanged", "06r": "changed", "dev-new": "new", "dev-old": "missing"}
	for name, k := range want {
		if kinds[name] != k {
			t.Fatalf("%s: kind %q, want %q (all: %v)", name, kinds[name], k, kinds)
		}
	}
	if _, ok := kinds["0vt"]; ok {
		t.Fatalf("0vt is on an unscanned server and must be left out, got %q", kinds["0vt"])
	}
}

func TestWBOURLsFor(t *testing.T) {
	// Real nginx names for env 06r: public, a second public domain, an internal one.
	domains := []string{"pos-staging-06r.parkee.app", "pos-staging-06r.pos-parkee.local", "pos-staging-06r.sistemparkiran.com"}
	got := wboURLsFor(domains, "https://x-{env}.example.com/details.json", "06r")
	if len(got) != 1 || got[0] != "https://pos-staging-06r.sistemparkiran.com/details.json" {
		t.Fatalf("want the sistemparkiran.com domain only, got %v", got)
	}
	got = wboURLsFor([]string{"a.pos-parkee.local"}, "https://pos-staging-automation-{env}.sistemparkiran.com/details.json", "0vt")
	if len(got) != 1 || got[0] != "https://pos-staging-automation-0vt.sistemparkiran.com/details.json" {
		t.Fatalf("want template fallback, got %v", got)
	}
	if got := wboURLsFor(nil, "", "x"); got != nil {
		t.Fatalf("no domain and no template must give no URL, got %v", got)
	}
}

func TestProbeDetailsVersionAndFailures(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"v1.18.0-44924546","location_name":"unknown","location_code":"unknown"}`))
	}))
	defer ok.Close()
	if r := probeDetails(context.Background(), ok.URL+"/details.json"); !r.Up || r.Version != "v1.18.0-44924546" {
		t.Fatalf("details.json should be up with version, got %+v", r)
	}
	notFound := httptest.NewServer(http.NotFoundHandler())
	defer notFound.Close()
	if r := probeDetails(context.Background(), notFound.URL); r.Up || r.Status != "HTTP 404" {
		t.Fatalf("404 must be down, got %+v", r)
	}
	// An SPA fallback answers 200 with HTML: still up, but no version.
	spa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html></html>"))
	}))
	defer spa.Close()
	if r := probeDetails(context.Background(), spa.URL); !r.Up || r.Version != "" {
		t.Fatalf("html fallback should be up with empty version, got %+v", r)
	}
}
