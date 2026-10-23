// Package config loads the platform configuration from a JSON file.
package config

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// StatusSeconds is how often every known env is curled for up/down.
	StatusSeconds int `json:"statusSeconds"`
	// WBOURL is the details.json URL used for an env when nginx has no domain
	// for it; "{env}" is replaced by the env name.
	WBOURL string `json:"wboUrl"`
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
	// Services deployable via Jenkins.
	Services []DeployService `json:"services"`
	// PollSeconds: how often to poll Jenkins for build status.
	PollSeconds int `json:"pollSeconds"`
	// RequireBooking: block deploy unless the user holds an active booking.
	RequireBooking bool `json:"requireBooking"`
}

// AutomationSlot is a staging-automation env used only to seed the env
// inventory on first start, before an admin has run discovery.
type AutomationSlot struct {
	Name string `json:"name"` // e.g. "0vt"
	// Ports maps a service name (as in discovery.services) to its published
	// port on the automation host, e.g. {"watersheep": 9004, "fisherman": 4004}.
	Ports map[string]int `json:"ports"`
}

// AutomationSSH is how discovery reaches the automation server: it runs
// `ssh <user>@<host> docker ps -a ...` with the system ssh client.
type AutomationSSH struct {
	User string `json:"user"`
	Port int    `json:"port"`
	// KeyPath is the private key file. Empty = ssh's default (~/.ssh/id_*),
	// which works when the platform runs directly on the host as that user.
	KeyPath string `json:"keyPath"`
	// KnownHostsPath stores the automation server's host key (learned on the
	// first connection). Defaults to known_hosts next to the database.
	KnownHostsPath string `json:"knownHostsPath"`
	Bin            string `json:"bin"`
}

// Automation describes the staging-automation server (separate from the
// staging server this platform runs on). Deploys to these envs use the
// "<job>-automation" Jenkins jobs.
type Automation struct {
	// Host is the IP/hostname used for both SSH discovery and status curls.
	Host string        `json:"host"`
	SSH  AutomationSSH `json:"ssh"`
	// ContainerInfix sits between the service and env name in container
	// names on that server: watersheep-automation-0vt -> "automation-".
	ContainerInfix string `json:"containerInfix"`
	// WBOURL is the WBO details.json URL per env; "{env}" is replaced by the
	// env name, e.g. https://pos-staging-automation-{env}.sistemparkiran.com/details.json
	WBOURL string `json:"wboUrl"`
	// Slots seeds the inventory on first start (see AutomationSlot).
	Slots []AutomationSlot `json:"slots"`
}

// APIKey is a static key for the machine-facing /api/v1 endpoints. Only the
// SHA-256 of the key is stored, so a leaked config.json does not reveal it.
type APIKey struct {
	// Name identifies the key holder in logs (not secret).
	Name string `json:"name"`
	// Hash is "sha256:<64 hex chars>" of the raw key.
	Hash string `json:"hash"`
	// Scopes limits what the key may call, e.g. ["bookings:read"].
	Scopes []string `json:"scopes"`
}

type API struct {
	Keys []APIKey `json:"keys"`
}

type Config struct {
	Listen      string     `json:"listen"`
	DBPath      string     `json:"dbPath"`
	Discovery   Discovery  `json:"discovery"`
	Automation  Automation `json:"automation"`
	Auth        Auth       `json:"auth"`
	Deploy      Deploy     `json:"deploy"`
	API         API        `json:"api"`
	HiddenSlots []string   `json:"hiddenSlots"`
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
	if c.Discovery.StatusSeconds <= 0 {
		c.Discovery.StatusSeconds = 120
	}
	if c.DBPath == "" {
		c.DBPath = "data/platform.db"
	}
	if c.Automation.SSH.User == "" {
		c.Automation.SSH.User = "parkee"
	}
	if c.Automation.SSH.Port <= 0 {
		c.Automation.SSH.Port = 22
	}
	if c.Automation.SSH.Bin == "" {
		c.Automation.SSH.Bin = "ssh"
	}
	if c.Automation.SSH.KnownHostsPath == "" {
		c.Automation.SSH.KnownHostsPath = filepath.Join(filepath.Dir(c.DBPath), "known_hosts")
	}
	if c.Automation.WBOURL == "" {
		c.Automation.WBOURL = "https://pos-staging-automation-{env}.sistemparkiran.com/details.json"
	}
	if c.Automation.ContainerInfix == "" {
		c.Automation.ContainerInfix = "automation-"
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
	if err := c.validateAPI(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}

// validateAPI rejects malformed API key entries at startup rather than
// silently never matching (or worse, matching an empty hash).
func (c *Config) validateAPI() error {
	seenName := map[string]bool{}
	seenHash := map[string]bool{}
	for i := range c.API.Keys {
		k := &c.API.Keys[i]
		if k.Name == "" {
			return fmt.Errorf("api.keys[%d]: name is required", i)
		}
		if seenName[k.Name] {
			return fmt.Errorf("api.keys[%d]: duplicate name %q", i, k.Name)
		}
		seenName[k.Name] = true
		h := strings.ToLower(strings.TrimPrefix(k.Hash, "sha256:"))
		if len(h) != 64 {
			return fmt.Errorf("api.keys[%d] (%s): hash must be \"sha256:\" + 64 hex chars", i, k.Name)
		}
		if _, err := hex.DecodeString(h); err != nil {
			return fmt.Errorf("api.keys[%d] (%s): hash is not valid hex", i, k.Name)
		}
		if seenHash[h] {
			return fmt.Errorf("api.keys[%d] (%s): duplicate hash", i, k.Name)
		}
		seenHash[h] = true
		k.Hash = "sha256:" + h
		if len(k.Scopes) == 0 {
			return fmt.Errorf("api.keys[%d] (%s): at least one scope is required", i, k.Name)
		}
	}
	return nil
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
