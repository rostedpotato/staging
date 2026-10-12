package discovery

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// slotPort reads the host-published port of a service from the slot's own
// .env, e.g. WATERSHEEP_PORT=9020 for watersheep. Returns "" if missing or
// not a valid port.
func slotPort(slotDir, service string) string {
	v := envValue(filepath.Join(slotDir, ".env"), strings.ToUpper(service)+"_PORT")
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return ""
	}
	return strconv.Itoa(n)
}

// envValue returns the value of KEY in a dotenv-style file, or "".
func envValue(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}
