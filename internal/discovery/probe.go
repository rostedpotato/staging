package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

// probeTimeout bounds one /actuator/info request. The staging host runs hot
// (load ~10), and a JVM service answering in 1-2s is still alive.
const probeTimeout = 3 * time.Second

var probeClient = &http.Client{Timeout: probeTimeout}

type probeResult struct {
	Up      bool
	Version string // build.version from /actuator/info
	Status  string // human line shown on the dashboard
}

// probeActuator does the equivalent of
// `curl http://<host>:<port>/actuator/info`: HTTP 200 means the service is up,
// anything else (refused, timeout, non-2xx) means down.
func probeActuator(ctx context.Context, host, port string) probeResult {
	url := "http://" + net.JoinHostPort(host, port) + "/actuator/info"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return probeResult{Status: "bad probe url"}
	}
	start := time.Now()
	resp, err := probeClient.Do(req)
	if err != nil {
		return probeResult{Status: "no response: " + probeReason(err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return probeResult{Status: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	res := probeResult{Up: true, Status: fmt.Sprintf("responding (%dms)", time.Since(start).Milliseconds())}
	var body struct {
		Build struct {
			Version string `json:"version"`
		} `json:"build"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body) == nil {
		res.Version = body.Build.Version
	}
	return res
}

// probeDetails curls a WBO details.json (e.g. https://pos-staging-06r.../details.json).
// A 2xx answer means the site is served; the version is the top-level
// "version" field. A site that answers 2xx without that JSON (an SPA fallback
// page) still counts as up, with an empty version.
func probeDetails(ctx context.Context, url string) probeResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return probeResult{Status: "bad probe url"}
	}
	start := time.Now()
	resp, err := probeClient.Do(req)
	if err != nil {
		return probeResult{Status: "no response: " + probeReason(err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return probeResult{Status: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	res := probeResult{Up: true, Status: fmt.Sprintf("responding (%dms)", time.Since(start).Milliseconds())}
	var body struct {
		Version string `json:"version"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body) == nil {
		res.Version = body.Version
	}
	return res
}

func probeReason(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	default:
		return "unreachable"
	}
}
