package discovery

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"parkee/staging-platform/internal/config"
)

func portOf(t *testing.T, addr string) string {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestScanProbesActuator(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/actuator/info" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"build":{"version":"v3.10.0-abc"}}`))
	}))
	defer up.Close()

	// A port that was just closed: connection refused.
	closed := httptest.NewServer(http.NotFoundHandler())
	closedPort := portOf(t, closed.Listener.Addr().String())
	closed.Close()

	root := t.TempDir()
	slotDir := filepath.Join(root, "agent-dev-01")
	if err := os.MkdirAll(filepath.Join(slotDir, "parkee-agent-backoffice"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := "WATERSHEEP_PORT=" + portOf(t, up.Listener.Addr().String()) + "\nFISHERMAN_PORT=" + closedPort + "\n"
	if err := os.WriteFile(filepath.Join(slotDir, ".env"), []byte(env), 0o644); err != nil {
		t.Fatal(err)
	}

	d := New(config.Discovery{
		AppRoot: root, SlotDirPrefix: "agent-", RepoSubdir: "parkee-agent-backoffice",
		NginxSitesDir: t.TempDir(), Services: []string{"watersheep", "fisherman"},
		ProbeHost: "127.0.0.1",
	})
	slots, err := d.Scan(context.Background(), nil)
	if err != nil || len(slots) != 1 {
		t.Fatalf("scan: slots=%d err=%v", len(slots), err)
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

func TestMissingPortIsDown(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agent-x"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := New(config.Discovery{
		AppRoot: root, SlotDirPrefix: "agent-", RepoSubdir: "r",
		NginxSitesDir: t.TempDir(), Services: []string{"watersheep"}, ProbeHost: "127.0.0.1",
	})
	slots, _ := d.Scan(context.Background(), nil)
	svc := slots[0].Services[0]
	if svc.State != "down" || svc.Status != "no WATERSHEEP_PORT in .env" {
		t.Fatalf("unexpected: %+v", svc)
	}
}
