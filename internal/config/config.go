// Package config loads the platform configuration from a JSON file.
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

type Discovery struct {
	AppRoot        string   `json:"appRoot"`
	SlotDirPrefix  string   `json:"slotDirPrefix"`
	RepoSubdir     string   `json:"repoSubdir"`
	NginxSitesDir  string   `json:"nginxSitesDir"`
	DockerBin      string   `json:"dockerBin"`
	Services       []string `json:"services"`
	RefreshSeconds int      `json:"refreshSeconds"`
	// ProbeHost is where service ports are curled for /actuator/info. From
	// inside the platform container this must be the Docker host
	// ("host.docker.internal"); use "127.0.0.1" when running on the host.
	ProbeHost string `json:"probeHost"`
}

type Auth struct {
	// SessionTTLHours controls how long a login session lasts.
	SessionTTLHours int `json:"sessionTTLHours"`
	// Secure marks session cookies Secure (set true when served over HTTPS).
	Secure bool `json:"secure"`
}

// DeployService maps a logical service to its Jenkins job and allowed refs.
type DeployService struct {
	Key      string `json:"key"`      // ws | fs | wbo
	Label    string `json:"label"`    // display name
	Job      string `json:"job"`      // Jenkins job name, e.g. deploy-ws
	TagParam bool   `json:"tagParam"` // whether TAG is allowed (WBO = false)
}

type Deploy struct {
	// Enabled turns the deploy feature on. If false, UI shows it as unavailable.
	Enabled bool `json:"enabled"`
	// JenkinsURL base, e.g. https://jenkins-agent.sistemparkiran.com
	JenkinsURL string `json:"jenkinsURL"`
	// JenkinsUser + JenkinsToken for Basic Auth (token is a SECRET). Shared by
	// all deploy jobs.
	JenkinsUser  string `json:"jenkinsUser"`
	JenkinsToken string `json:"jenkinsToken"`
	// Slots that may be deployed (defaults to discovered slots if empty).
	Slots []string `json:"slots"`
	// AutomationSlots live on a separate host (staging-automation) that this
	// platform instance never scans, so they're listed manually here instead
	// of being discovered. Deploying to one of these slots targets the
	// "<job>-automation" Jenkins job instead of the regular staging job, and
	// skips the booking requirement (no reservation system on that host yet).
	AutomationSlots []string `json:"automationSlots"`
	// Services deployable via Jenkins.
	Services []DeployService `json:"services"`
	// PollSeconds: how often to poll Jenkins for build status.
	PollSeconds int `json:"pollSeconds"`
	// RequireBooking: block deploy unless the user holds an active booking.
	RequireBooking bool `json:"requireBooking"`
}

// AutomationHost configures read-only SSH access to the staging-automation
// server so its slots (0pv/0vt/076, on a host this platform instance never
// scans directly) can be shown on the same dashboard. The same SSH
// connection is also the intended path for a later "stop idle env" action.
type AutomationHost struct {
	// Enabled turns remote scanning on. If false, the dashboard simply omits
	// the staging-automation section.
	Enabled bool `json:"enabled"`
	// Addr is host:port of the SSH server, e.g. "parkee-agent-automation:22".
	Addr string `json:"addr"`
	// User is the SSH username (should be a restricted, read-only account).
	User string `json:"user"`
	// PrivateKeyPath points at a PEM-encoded private key file (not the key
	// content itself - never put key material directly in config.json).
	PrivateKeyPath string `json:"privateKeyPath"`
	// HostKey is the expected SSH host public key, in authorized_keys format
	// (e.g. "ssh-ed25519 AAAA..."), used to pin the connection instead of
	// trusting whatever key the server presents. Required when Enabled.
	HostKey string `json:"hostKey"`
	// Discovery mirrors the local Discovery config but describes the layout
	// on the REMOTE host (its own appRoot/slotDirPrefix/services may differ).
	Discovery Discovery `json:"discovery"`
	// RefreshSeconds: how often to re-run the remote scan (separate from the
	// local discovery.refreshSeconds since it's a slower network operation).
	RefreshSeconds int `json:"refreshSeconds"`
	// TimeoutSeconds bounds each SSH connection+command round trip so a
	// hung/unreachable automation host can never block the scan loop.
	TimeoutSeconds int `json:"timeoutSeconds"`
}

type Config struct {
	Listen         string         `json:"listen"`
	DBPath         string         `json:"dbPath"`
	Discovery      Discovery      `json:"discovery"`
	AutomationHost AutomationHost `json:"automationHost"`
	Auth           Auth           `json:"auth"`
	Deploy         Deploy         `json:"deploy"`
	HiddenSlots    []string       `json:"hiddenSlots"`
}

// withDefaults fills sensible defaults so a partial config still works.
func (c *Config) withDefaults() {
	if c.Listen == "" {
		c.Listen = ":8088"
	}
	if c.Discovery.AppRoot == "" {
		c.Discovery.AppRoot = "/data/app"
	}
	if c.Discovery.SlotDirPrefix == "" {
		c.Discovery.SlotDirPrefix = "agent-"
	}
	if c.Discovery.RepoSubdir == "" {
		c.Discovery.RepoSubdir = "parkee-agent-backoffice"
	}
	if c.Discovery.NginxSitesDir == "" {
		c.Discovery.NginxSitesDir = "/etc/nginx/sites-enabled"
	}
	if c.Discovery.DockerBin == "" {
		c.Discovery.DockerBin = "docker"
	}
	if len(c.Discovery.Services) == 0 {
		c.Discovery.Services = []string{"watersheep", "fisherman"}
	}
	if c.Discovery.RefreshSeconds <= 0 {
		c.Discovery.RefreshSeconds = 900
	}
	if c.Discovery.ProbeHost == "" {
		c.Discovery.ProbeHost = "host.docker.internal"
	}
	if c.DBPath == "" {
		c.DBPath = "data/platform.db"
	}
	if c.Auth.SessionTTLHours <= 0 {
		c.Auth.SessionTTLHours = 12
	}
	if c.Deploy.PollSeconds <= 0 {
		c.Deploy.PollSeconds = 5
	}
	if len(c.Deploy.Services) == 0 {
		c.Deploy.Services = []DeployService{
			{Key: "ws", Label: "Watersheep", Job: "deploy-ws", TagParam: true},
			{Key: "fs", Label: "Fisherman", Job: "deploy-fs", TagParam: true},
			{Key: "wbo", Label: "Web Backoffice", Job: "deploy-wbo", TagParam: false},
		}
	}
	if c.AutomationHost.RefreshSeconds <= 0 {
		c.AutomationHost.RefreshSeconds = 900
	}
	if c.AutomationHost.TimeoutSeconds <= 0 {
		c.AutomationHost.TimeoutSeconds = 20
	}
	if c.AutomationHost.Discovery.SlotDirPrefix == "" {
		c.AutomationHost.Discovery.SlotDirPrefix = "agent-"
	}
	if c.AutomationHost.Discovery.RepoSubdir == "" {
		c.AutomationHost.Discovery.RepoSubdir = "parkee-agent-backoffice"
	}
	if len(c.AutomationHost.Discovery.Services) == 0 {
		c.AutomationHost.Discovery.Services = []string{"watersheep", "fisherman"}
	}
}

// ServiceByKey returns the deploy service config for a key (ws/fs/wbo).
func (c *Config) ServiceByKey(key string) *DeployService {
	for i := range c.Deploy.Services {
		if c.Deploy.Services[i].Key == key {
			return &c.Deploy.Services[i]
		}
	}
	return nil
}

// IsAutomationSlot reports whether slot belongs to the staging-automation host.
func (c *Config) IsAutomationSlot(slot string) bool {
	for _, s := range c.Deploy.AutomationSlots {
		if s == slot {
			return true
		}
	}
	return false
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	c.withDefaults()
	return &c, nil
}

// Hidden reports whether a slot name is configured to be hidden.
func (c *Config) Hidden(slot string) bool {
	for _, h := range c.HiddenSlots {
		if h == slot {
			return true
		}
	}
	return false
}
