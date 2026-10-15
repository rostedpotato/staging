package discovery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"parkee/staging-platform/internal/config"
	"parkee/staging-platform/internal/store"
)

// fakeDocker writes a script that logs every call and answers `inspect` the
// way docker does for compose containers ("/name|project").
func fakeDocker(t *testing.T, inspectOut string) (bin, logFile string) {
	t.Helper()
	dir := t.TempDir()
	logFile = filepath.Join(dir, "calls.log")
	bin = filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logFile + "\n" +
		"if [ \"$1\" = inspect ]; then printf '%s' '" + inspectOut + "'; fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logFile
}

func calls(t *testing.T, logFile string) []string {
	t.Helper()
	b, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

const twoContainers = "/watersheep-dev-04|proj04\n/fisherman-dev-04|proj04\n"

func newTestDisc(bin string) *Discoverer {
	return New(config.Discovery{DockerBin: bin, Services: []string{"watersheep", "fisherman"}})
}

// The whole env is one `docker compose -p <project> <verb>`: same as running
// `docker compose stop|start` in the env folder, no service list, no directory.
func TestStopEnv_RunsComposeStopForTheWholeProject(t *testing.T) {
	bin, logFile := fakeDocker(t, twoContainers)
	if err := newTestDisc(bin).StopEnv(context.Background(), store.ServerStaging, "dev-04", config.Automation{}); err != nil {
		t.Fatal(err)
	}
	got := calls(t, logFile)
	if len(got) != 2 || got[1] != "compose -p proj04 stop" {
		t.Fatalf("want inspect then 'compose -p proj04 stop', got %v", got)
	}
}

func TestStartEnv_RunsComposeStartForTheWholeProject(t *testing.T) {
	bin, logFile := fakeDocker(t, twoContainers)
	if err := newTestDisc(bin).StartEnv(context.Background(), store.ServerStaging, "dev-04", config.Automation{}); err != nil {
		t.Fatal(err)
	}
	got := calls(t, logFile)
	if len(got) != 2 || got[1] != "compose -p proj04 start" {
		t.Fatalf("want inspect then 'compose -p proj04 start', got %v", got)
	}
}

func TestControl_NeverRunsDownOrRemovesAnything(t *testing.T) {
	bin, logFile := fakeDocker(t, twoContainers)
	d := newTestDisc(bin)
	_ = d.StopEnv(context.Background(), store.ServerStaging, "dev-04", config.Automation{})
	_ = d.StartEnv(context.Background(), store.ServerStaging, "dev-04", config.Automation{})
	for _, c := range calls(t, logFile) {
		for _, bad := range []string{" down", " rm", " -v", " restart", " kill"} {
			if strings.Contains(c, bad) {
				t.Fatalf("forbidden command %q in %q", bad, c)
			}
		}
	}
}

func TestControl_RefusesWhenNothingSafeToDo(t *testing.T) {
	cases := map[string]string{
		"missing container": "/watersheep-dev-04|proj04\n",
		"not compose":       "/watersheep-dev-04|\n/fisherman-dev-04|\n",
		"two projects":      "/watersheep-dev-04|a\n/fisherman-dev-04|b\n",
		"hostile label":     "/watersheep-dev-04|a;rm -rf /\n/fisherman-dev-04|a;rm -rf /\n",
	}
	for name, inspect := range cases {
		t.Run(name, func(t *testing.T) {
			bin, logFile := fakeDocker(t, inspect)
			if err := newTestDisc(bin).StopEnv(context.Background(), store.ServerStaging, "dev-04", config.Automation{}); err == nil {
				t.Fatal("expected an error")
			}
			for _, c := range calls(t, logFile) {
				if strings.HasPrefix(c, "compose") {
					t.Fatalf("nothing may be stopped, but ran %q", c)
				}
			}
		})
	}
}

func TestControl_RejectsBadEnvName(t *testing.T) {
	d := New(config.Discovery{DockerBin: "true", Services: []string{"watersheep"}})
	for _, env := range []string{"", "a b", "x;y", "../x", "$(id)"} {
		if err := d.StopEnv(context.Background(), store.ServerStaging, env, config.Automation{}); err == nil {
			t.Fatalf("env %q should be rejected", env)
		}
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Fatalf("got %s", got)
	}
}

func svc(name, state string) Service {
	return Service{Name: name, Found: true, State: state, HostPort: "1"}
}

func wbo(state string) Service {
	return Service{Name: "wbo", Found: true, State: state, wboURLs: []string{"https://x/details.json"}}
}

func TestOverall_StoppedEnvIsNotRed(t *testing.T) {
	s := Slot{Desired: "stopped", Services: []Service{svc("watersheep", "down"), svc("fisherman", "down"), wbo("running")}}
	s.markStopped()
	if got := s.Overall(); got != "stopped" {
		t.Fatalf("want stopped, got %s", got)
	}
	if s.Services[2].State != "running" {
		t.Fatal("WBO must stay green on a stopped env")
	}
}

func TestOverall_StoppedButWBODownIsRed(t *testing.T) {
	s := Slot{Desired: "stopped", Services: []Service{svc("watersheep", "down"), svc("fisherman", "down"), wbo("down")}}
	s.markStopped()
	if got := s.Overall(); got != "down" {
		t.Fatalf("want down, got %s", got)
	}
}

func TestOverall_StoppedButAppStillRunningIsShown(t *testing.T) {
	// e.g. Jenkins restarted fisherman: do not hide it behind "stopped".
	s := Slot{Desired: "stopped", Services: []Service{svc("watersheep", "down"), svc("fisherman", "running"), wbo("running")}}
	s.markStopped()
	if got := s.Overall(); got == "stopped" {
		t.Fatalf("a running app on a 'stopped' env must not be reported as stopped")
	}
}

func TestOverall_RunningEnvDownStaysRed(t *testing.T) {
	s := Slot{Desired: "running", Services: []Service{svc("watersheep", "down"), svc("fisherman", "running"), wbo("running")}}
	s.markStopped()
	if got := s.Overall(); got != "down" {
		t.Fatalf("want down, got %s", got)
	}
}

func TestOverall_TransitionWins(t *testing.T) {
	s := Slot{Transition: "starting", Services: []Service{svc("watersheep", "down")}}
	if got := s.Overall(); got != "starting" {
		t.Fatalf("want starting, got %s", got)
	}
}
