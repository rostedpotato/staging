package discovery

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"parkee/staging-platform/internal/config"
	"parkee/staging-platform/internal/store"
)

// dockerPsFormat prints one "name|ports" line per container. It's the only
// command discovery runs, locally and over SSH.
const dockerPsFormat = "{{.Names}}|{{.Ports}}"

// discoverTimeout bounds one docker ps (local or over SSH). The staging host
// has ~50 containers under heavy load; ps there can take tens of seconds.
const discoverTimeout = 90 * time.Second

var envNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// parseDockerPs turns "name|ports" lines into envs. A container named
// "<service><infix><env>" (e.g. watersheep-automation-0vt with infix
// "-automation-", or watersheep-ho with infix "-") contributes its first
// published host port to that env. Containers of other services (kafka,
// postgres, ...) are ignored.
func parseDockerPs(out, server, infix string, services []string) []store.EnvChange {
	envs := map[string]map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		name, ports, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok {
			continue
		}
		for _, svc := range services {
			prefix := svc + infix
			env, ok := strings.CutPrefix(name, prefix)
			if !ok || !envNameRe.MatchString(env) {
				continue
			}
			if envs[env] == nil {
				envs[env] = map[string]int{}
			}
			if p, err := strconv.Atoi(firstHostPort(ports)); err == nil {
				envs[env][svc] = p
			}
		}
	}
	out2 := make([]store.EnvChange, 0, len(envs))
	for name, ports := range envs {
		out2 = append(out2, store.EnvChange{Name: name, Server: server, Ports: ports})
	}
	sort.Slice(out2, func(i, j int) bool { return out2[i].Name < out2[j].Name })
	return out2
}

// firstHostPort pulls the first published host port from a docker Ports string
// like "0.0.0.0:9020->9005/tcp, :::9020->9005/tcp" (or "10.20.1.12:10007->9092/tcp").
func firstHostPort(ports string) string {
	for _, part := range strings.Split(ports, ",") {
		hostSide, _, ok := strings.Cut(strings.TrimSpace(part), "->")
		if !ok {
			continue
		}
		if i := strings.LastIndex(hostSide, ":"); i >= 0 {
			return hostSide[i+1:]
		}
	}
	return ""
}

// DiscoverStaging lists envs on the staging server from the local docker.
func (d *Discoverer) DiscoverStaging(ctx context.Context) ([]store.EnvChange, error) {
	out, err := run(ctx, d.cfg.DockerBin, "ps", "-a", "--format", dockerPsFormat)
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	return parseDockerPs(out, store.ServerStaging, "-", d.cfg.Services), nil
}

// DiscoverAutomation lists envs on the automation server by running docker ps
// over SSH. BatchMode makes ssh fail instead of prompting for a password.
// The host key is learned on first connect and pinned in KnownHostsPath.
func (d *Discoverer) DiscoverAutomation(ctx context.Context, cfg config.Automation) ([]store.EnvChange, error) {
	args, err := sshArgs(cfg, "docker ps -a --format '"+dockerPsFormat+"'")
	if err != nil {
		return nil, err
	}
	out, err := run(ctx, cfg.SSH.Bin, args...)
	if err != nil {
		return nil, fmt.Errorf("ssh %s@%s: %w", cfg.SSH.User, cfg.Host, err)
	}
	return parseDockerPs(out, store.ServerAutomation, "-"+cfg.ContainerInfix, d.cfg.Services), nil
}

// sshArgs builds the ssh argument list that runs remoteCmd on the automation
// server. The remote shell receives remoteCmd as one string, so the caller is
// responsible for quoting anything inside it. BatchMode makes ssh fail instead
// of prompting; the host key is learned on first connect and pinned.
func sshArgs(cfg config.Automation, remoteCmd string) ([]string, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("automation.host is not set")
	}
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + cfg.SSH.KnownHostsPath,
		"-p", strconv.Itoa(cfg.SSH.Port),
	}
	if cfg.SSH.KeyPath != "" {
		args = append(args, "-i", cfg.SSH.KeyPath, "-o", "IdentitiesOnly=yes")
	}
	return append(args, cfg.SSH.User+"@"+cfg.Host, remoteCmd), nil
}

// run executes a command with discoverTimeout and folds stderr into the error.
func run(ctx context.Context, bin string, args ...string) (string, error) {
	return runT(ctx, discoverTimeout, bin, args...)
}

func runT(ctx context.Context, timeout time.Duration, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, lastLine(msg))
		}
		return "", err
	}
	return stdout.String(), nil
}

func lastLine(s string) string {
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}
