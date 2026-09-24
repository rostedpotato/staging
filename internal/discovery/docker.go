package discovery

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"
)

// dockerContainer is the subset of `docker ps` JSON output we use.
type dockerContainer struct {
	Names  string `json:"Names"`
	Image  string `json:"Image"`
	State  string `json:"State"`
	Status string `json:"Status"`
	Ports  string `json:"Ports"`
}

// listContainers runs `docker ps -a --format '{{json .}}'` (read-only) and
// returns all containers. Each line of output is a JSON object.
func (d *Discoverer) listContainers(ctx context.Context) ([]dockerContainer, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, d.cfg.DockerBin, "ps", "-a",
		"--no-trunc", "--format", "{{json .}}").Output()
	if err != nil {
		return nil, err
	}
	var res []dockerContainer
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var c dockerContainer
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			continue // skip malformed lines rather than fail the whole scan
		}
		res = append(res, c)
	}
	return res, nil
}

// health extracts the health hint ("healthy"/"unhealthy"/"starting") from a
// docker status string like "Up 2 hours (healthy)".
func health(status string) string {
	l := strings.ToLower(status)
	switch {
	case strings.Contains(l, "(healthy)"):
		return "healthy"
	case strings.Contains(l, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(l, "(health: starting)"), strings.Contains(l, "starting"):
		return "starting"
	default:
		return "none"
	}
}

// firstHostPort pulls the first published host port from a docker Ports string
// like "0.0.0.0:9020->9005/tcp, :::9020->9005/tcp".
func firstHostPort(ports string) string {
	for _, part := range strings.Split(ports, ",") {
		part = strings.TrimSpace(part)
		if i := strings.Index(part, "->"); i > 0 {
			hostSide := part[:i]
			if j := strings.LastIndex(hostSide, ":"); j >= 0 {
				return hostSide[j+1:]
			}
		}
	}
	return ""
}
