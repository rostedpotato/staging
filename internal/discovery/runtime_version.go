package discovery

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

const actuatorTimeout = 2 * time.Second

var actuatorClient = &http.Client{Timeout: actuatorTimeout}

// runtimeVersion asks a running service for the version it's actually
// serving, via its Spring Boot Actuator endpoint (GET /actuator/info ->
// build.version). This is reached through host.docker.internal since the
// platform container sits on its own Docker network and can't otherwise see
// another container's host-published port.
func runtimeVersion(ctx context.Context, hostPort string) string {
	if hostPort == "" {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://host.docker.internal:"+hostPort+"/actuator/info", nil)
	if err != nil {
		return ""
	}
	resp, err := actuatorClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		Build struct {
			Version string `json:"version"`
		} `json:"build"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ""
	}
	return body.Build.Version
}
