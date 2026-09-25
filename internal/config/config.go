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

type Config struct {
	Listen      string    `json:"listen"`
	DBPath      string    `json:"dbPath"`
	Discovery   Discovery `json:"discovery"`
	Auth        Auth      `json:"auth"`
	Deploy      Deploy    `json:"deploy"`
	HiddenSlots []string  `json:"hiddenSlots"`
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
