package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"parkee/staging-platform/internal/config"
)

// RemoteDiscoverer scans a slot layout on a different host over SSH (used
// for staging-automation, which this process never has local filesystem or
// docker.sock access to). It runs a single shell script per scan so the
// docker/git/nginx snapshot it returns is internally consistent, then parses
// the script's JSON output into the same Slot/Service model as the local
// Discoverer - callers and templates don't need to know the data's origin.
type RemoteDiscoverer struct {
	cfg config.AutomationHost
}

func NewRemote(cfg config.AutomationHost) *RemoteDiscoverer { return &RemoteDiscoverer{cfg: cfg} }

// remoteSlot/remoteService mirror Slot/Service but are the exact JSON shape
// emitted by remoteScanScript, kept separate from the public model so the
// wire format can evolve independently of it.
type remoteService struct {
	Name     string `json:"name"`
	Found    bool   `json:"found"`
	State    string `json:"state"`
	Status   string `json:"status"`
	Health   string `json:"health"`
	HostPort string `json:"hostPort"`
	Image    string `json:"image"`
}

type remoteSlot struct {
	Name       string          `json:"name"`
	Dir        string          `json:"dir"`
	Branch     string          `json:"branch"`
	Commit     string          `json:"commit"`
	CommitTime string          `json:"commitTime"`
	Domains    []string        `json:"domains"`
	Services   []remoteService `json:"services"`
}

// Scan connects over SSH, runs the remote probe script, and returns slots in
// the same shape local discovery produces. Every step has its own timeout
// (config.AutomationHost.TimeoutSeconds) so a hung or unreachable host can
// never block the caller's refresh loop indefinitely.
func (d *RemoteDiscoverer) Scan(ctx context.Context) ([]Slot, error) {
	if !d.cfg.Enabled {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(d.cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	client, err := d.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("automation host dial: %w", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("automation host session: %w", err)
	}
	defer session.Close()

	var out, errOut strings.Builder
	session.Stdout = &out
	session.Stderr = &errOut

	script := remoteScanScript(d.cfg.Discovery)
	runErr := make(chan error, 1)
	go func() { runErr <- session.Run(script) }()

	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		return nil, fmt.Errorf("automation host scan timed out: %w", ctx.Err())
	case err := <-runErr:
		if err != nil {
			return nil, fmt.Errorf("automation host scan failed: %w (stderr: %s)", err, strings.TrimSpace(errOut.String()))
		}
	}

	var remote []remoteSlot
	if err := json.Unmarshal([]byte(out.String()), &remote); err != nil {
		return nil, fmt.Errorf("automation host scan: parse output: %w", err)
	}

	slots := make([]Slot, 0, len(remote))
	for _, rs := range remote {
		services := make([]Service, 0, len(rs.Services))
		for _, svc := range rs.Services {
			services = append(services, Service{
				Name:     svc.Name,
				Found:    svc.Found,
				State:    svc.State,
				Status:   svc.Status,
				Health:   svc.Health,
				HostPort: svc.HostPort,
				Image:    svc.Image,
			})
		}
		version := rs.Branch
		if rs.Commit != "" {
			if version != "" {
				version += "-"
			}
			version += rs.Commit
		}
		slots = append(slots, Slot{
			Name:       rs.Name,
			Dir:        rs.Dir,
			Domains:    rs.Domains,
			Branch:     rs.Branch,
			Commit:     rs.Commit,
			Version:    version,
			CommitTime: rs.CommitTime,
			Services:   services,
		})
	}
	return slots, nil
}

// dial loads the private key and connects, pinning the host key against the
// configured fingerprint so a compromised/spoofed network path can't make
// this read a false "all healthy" (or, later, trigger a stop) on the wrong
// box.
func (d *RemoteDiscoverer) dial(ctx context.Context) (*ssh.Client, error) {
	keyBytes, err := os.ReadFile(d.cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	hostKeyCallback, err := pinnedHostKey(d.cfg.HostKey)
	if err != nil {
		return nil, fmt.Errorf("host key: %w", err)
	}

	clientCfg := &ssh.ClientConfig{
		User:            d.cfg.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         time.Duration(d.cfg.TimeoutSeconds) * time.Second,
	}

	type dialResult struct {
		client *ssh.Client
		err    error
	}
	resCh := make(chan dialResult, 1)
	go func() {
		c, err := ssh.Dial("tcp", d.cfg.Addr, clientCfg)
		resCh <- dialResult{c, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-resCh:
		return res.client, res.err
	}
}

// pinnedHostKey parses a single "ssh-ed25519 AAAA..." authorized_keys-format
// line and returns a callback that only accepts a connection presenting
// exactly that key, instead of trust-on-first-use.
func pinnedHostKey(line string) (ssh.HostKeyCallback, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, fmt.Errorf("automationHost.hostKey is required when automationHost.enabled is true")
	}
	pubKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return nil, fmt.Errorf("parse host key: %w", err)
	}
	return ssh.FixedHostKey(pubKey), nil
}
