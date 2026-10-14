package discovery

import (
	"os"
	"path/filepath"
	"testing"
)

// Server blocks copied from `nginx -T` on the staging server.
var realNginx = map[string]string{
	"portainer-ho.conf": "server {\n    listen 443 ssl ;\n    server_name pr-ho.sistemparkiran.com;\n    proxy_pass https://localhost:9443;\n}\n",
	"pos-staging-local.conf": "server {\n    listen 443 ssl;\n    server_name pos-staging-ho.pos-parkee.local;\n" +
		"    root /data/app/agent-ho/parkee-agent-backoffice/dist;\n}\n",
	"pos-staging-parkee.app.conf": "server {\n    listen 443 ssl ;\n    server_name pos-staging.parkee.app;\n    proxy_pass http://localhost:2000;\n}\n",
	"pos-staging-06r.conf": "server {\n    listen 443 ssl ;\n    server_name pos-staging-06r.sistemparkiran.com;\n" +
		"    root /data/app/agent-06r/parkee-agent-backoffice/dist;\n}\n",
	"pos-staging-06r-parkee-app.conf": "server {\n    listen 443 ssl ;\n    server_name pos-staging-06r.parkee.app;\n    proxy_pass http://localhost:2007;\n}\n",
	"pos-staging-06r-local.conf": "server {\n    listen 443 ssl;\n    server_name pos-staging-06r.pos-parkee.local;\n" +
		"    root /data/app/agent-06r/parkee-agent-backoffice/dist;\n}\n",
	"pos-staging-04": "server {\n    listen 443 ssl ;\n    server_name pos-staging-04.sistemparkiran.com;\n" +
		"    root /data/app/agent-development-04/parkee-agent-backoffice/dist;\n}\n",
}

func writeNginx(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range realNginx {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestWBODomainsOnlyFromRootMatch(t *testing.T) {
	dir := writeNginx(t)

	// The loose matcher (used for link display) lets "ho" pick up the
	// unrelated portainer-ho.conf by file name...
	loose := domainsBySlot(dir, "/data/app", "agent-", []string{"ho", "06r"})
	foundPortainer := false
	for _, d := range loose["ho"] {
		if d == "pr-ho.sistemparkiran.com" {
			foundPortainer = true
		}
	}
	if !foundPortainer {
		t.Fatalf("test premise changed: loose match for ho = %v", loose["ho"])
	}

	// ...but the WBO matcher must not: only configs rooted at the slot's dist.
	strict := wboDomainsBySlot(dir, "/data/app", "agent-")
	for _, d := range strict["ho"] {
		if d == "pr-ho.sistemparkiran.com" {
			t.Fatalf("WBO domains for ho must not include portainer, got %v", strict["ho"])
		}
	}
	if got := wboURLsFor(strict["06r"], "", "06r"); len(got) != 1 || got[0] != "https://pos-staging-06r.sistemparkiran.com/details.json" {
		t.Fatalf("06r should use its sistemparkiran.com domain, got %v", got)
	}
	if got := wboURLsFor(strict["development-04"], "", "development-04"); len(got) != 1 || got[0] != "https://pos-staging-04.sistemparkiran.com/details.json" {
		t.Fatalf("development-04 should use pos-staging-04, got %v", got)
	}
	// ho is served only on an internal .local name: no usable public URL.
	if got := wboURLsFor(strict["ho"], "", "ho"); got != nil {
		t.Fatalf("ho has no public domain, want no URL, got %v", got)
	}
}
