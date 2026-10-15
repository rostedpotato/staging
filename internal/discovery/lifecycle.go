package discovery

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"parkee/staging-platform/internal/config"
	"parkee/staging-platform/internal/store"
)

// controlTimeout bounds one compose stop/start. A JVM that ignores SIGTERM is
// killed after docker's 10s grace period (seen on the staging server: ~11s for
// each of watersheep and fisherman).
const controlTimeout = 3 * time.Minute

// composeNameRe is what a compose project / service name may look like. Both
// come from container labels and end up in a remote shell command over SSH,
// so anything unusual is rejected instead of quoted.
var composeNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// dockerFunc runs `docker <args>` on one server (locally, or over SSH).
type dockerFunc func(ctx context.Context, args ...string) (string, error)

// StopEnv switches a whole env off with `docker compose -p <project> stop`,
// the same as running `docker compose stop` in the env's folder. Compose
// stops containers in reverse depends_on order. Nothing is removed: never
// `down`, because Jenkins' `docker restart` needs the containers to exist.
//
// The project is read from the labels compose put on the env's containers, so
// no directory or project name is configured anywhere.
func (d *Discoverer) StopEnv(ctx context.Context, server, env string, auto config.Automation) error {
	return d.control(ctx, server, env, auto, "stop")
}

// StartEnv switches the whole env back on with `docker compose -p <project>
// start`, which starts containers in depends_on order (databases first).
func (d *Discoverer) StartEnv(ctx context.Context, server, env string, auto config.Automation) error {
	return d.control(ctx, server, env, auto, "start")
}

func (d *Discoverer) control(ctx context.Context, server, env string, auto config.Automation, verb string) error {
	if !envNameRe.MatchString(env) {
		return fmt.Errorf("invalid env name %q", env)
	}
	docker, infix, err := d.dockerFor(server, auto)
	if err != nil {
		return err
	}
	project, err := d.composeProject(ctx, docker, infix, env)
	if err != nil {
		return err
	}
	if _, err := docker(ctx, "compose", "-p", project, verb); err != nil {
		return fmt.Errorf("compose -p %s %s: %w", project, verb, err)
	}
	return nil
}

// dockerFor returns how to run docker on a server plus the container-name
// infix used there ("-" on staging, "-automation-" on automation).
func (d *Discoverer) dockerFor(server string, auto config.Automation) (dockerFunc, string, error) {
	switch server {
	case store.ServerStaging:
		return func(ctx context.Context, args ...string) (string, error) {
			return runT(ctx, controlTimeout, d.cfg.DockerBin, args...)
		}, "-", nil
	case store.ServerAutomation:
		return func(ctx context.Context, args ...string) (string, error) {
			quoted := make([]string, len(args))
			for i, a := range args {
				quoted[i] = shellQuote(a)
			}
			sshA, err := sshArgs(auto, "docker "+strings.Join(quoted, " "))
			if err != nil {
				return "", err
			}
			out, err := runT(ctx, controlTimeout, auto.SSH.Bin, sshA...)
			if err != nil {
				return "", fmt.Errorf("ssh %s@%s: %w", auto.SSH.User, auto.Host, err)
			}
			return out, nil
		}, "-" + auto.ContainerInfix, nil
	}
	return nil, "", fmt.Errorf("unknown server %q", server)
}

// composeProject finds the compose project of an env from the labels compose
// put on its app containers. This is what replaces "cd into the env folder":
// `docker compose -p <project> stop` finds the containers by label, so no
// directory, compose file, or project name has to be configured (0pv's project
// is named differently from 0vt's, which this handles for free).
//
// Every configured service container must exist and belong to the same
// project; otherwise nothing is stopped or started.
func (d *Discoverer) composeProject(ctx context.Context, docker dockerFunc, infix, env string) (string, error) {
	args := []string{"inspect", "--type", "container", "--format",
		`{{.Name}}|{{index .Config.Labels "com.docker.compose.project"}}`}
	names := make([]string, len(d.cfg.Services))
	for i, svc := range d.cfg.Services {
		names[i] = svc + infix + env
	}
	out, err := docker(ctx, append(args, names...)...)
	if err != nil {
		return "", fmt.Errorf("docker inspect %s: %w", strings.Join(names, ", "), err)
	}
	byName := map[string]string{} // container name -> compose project
	for _, line := range strings.Split(out, "\n") {
		name, project, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok {
			continue
		}
		byName[strings.TrimPrefix(name, "/")] = project
	}
	project := ""
	for _, name := range names {
		p, ok := byName[name]
		switch {
		case !ok:
			return "", fmt.Errorf("container %s not found", name)
		case p == "":
			return "", fmt.Errorf("container %s was not started by docker compose", name)
		case !composeNameRe.MatchString(p):
			return "", fmt.Errorf("container %s has an unexpected compose project label", name)
		case project != "" && project != p:
			return "", fmt.Errorf("env %s spans two compose projects (%s, %s); refusing", env, project, p)
		}
		project = p
	}
	return project, nil
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
