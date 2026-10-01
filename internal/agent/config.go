package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type ScrapeTarget struct {
	URL     string `json:"url"`
	Service string `json:"service"`
}

type LogSource struct {
	Paths   []string `json:"paths"`
	Service string   `json:"service"`
	Format  string   `json:"format"` // text | docker | nextcloud
}

// NextcloudTarget is one Nextcloud instance to monitor remotely (status.php + serverinfo API).
// Auth for the serverinfo API: either Token (occ config:app:set serverinfo token) or an
// admin Username + app Password. LogPath optionally tails that instance's nextcloud.log.
type NextcloudTarget struct {
	URL      string `json:"url"`
	Service  string `json:"service"`
	Token    string `json:"token"`
	Username string `json:"username"`
	Password string `json:"password"`
	LogPath  string `json:"log_path"`
}

type Config struct {
	URL             string            `json:"url"`
	APIKey          string            `json:"api_key"`
	HostService     string            `json:"host_service"`
	IntervalSeconds int               `json:"interval_seconds"`
	HostMetrics     *bool             `json:"host_metrics"` // default true
	SelfMetrics     *bool             `json:"self_metrics"` // default true
	MetricsListen   string            `json:"metrics_listen"`
	Scrape          []ScrapeTarget    `json:"scrape"`
	Logs            []LogSource       `json:"logs"`
	Nextcloud       []NextcloudTarget `json:"nextcloud"`
	Systemd         *bool             `json:"systemd"`        // report systemd services (default true)
	Containers      *bool             `json:"containers"`     // report Docker containers (default true)
	WatchServices   []string          `json:"watch_services"` // services that must be running
	NoRemote        bool              `json:"no_remote"`      // ignore the server's configuration
	// AllowedLogDirs limits which log paths the SERVER may tell this agent to read (paths in the local config file are
	// always trusted). Default: /var/log, Docker's data dirs, /var/www, /srv, /mnt, /opt. Use ["*"] to allow everything.
	AllowedLogDirs   []string `json:"allowed_log_dirs"`
	RejectedLogPaths []string `json:"-"` // remote paths that were refused, for the agent's log and the UI warning
}

func (c Config) HostOn() bool       { return c.HostMetrics == nil || *c.HostMetrics }
func (c Config) SelfOn() bool       { return c.SelfMetrics == nil || *c.SelfMetrics }
func (c Config) SystemdOn() bool    { return c.Systemd == nil || *c.Systemd }
func (c Config) ContainersOn() bool { return c.Containers == nil || *c.Containers }

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// LoadConfig reads an optional JSON file, then applies LUMEN_AGENT_* environment variables
// (used by the Docker agent, which has no config file), then defaults.
func LoadConfig(path string, getenv func(string) string) (Config, error) {
	var c Config
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("invalid config %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	if v := getenv("LUMEN_AGENT_URL"); v != "" {
		c.URL = v
	}
	if v := getenv("LUMEN_AGENT_API_KEY"); v != "" {
		c.APIKey = v
	}
	if truthy(getenv("LUMEN_AGENT_NO_REMOTE")) {
		c.NoRemote = true
	}
	if v := getenv("LUMEN_AGENT_SYSTEMD"); v != "" {
		b := truthy(v)
		c.Systemd = &b
	}
	if v := getenv("LUMEN_AGENT_CONTAINERS"); v != "" {
		b := truthy(v)
		c.Containers = &b
	}
	if v := getenv("LUMEN_AGENT_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.IntervalSeconds = n
		}
	}
	if v := getenv("LUMEN_AGENT_METRICS_LISTEN"); v != "" {
		c.MetricsListen = v
	}
	if v := getenv("LUMEN_AGENT_HOST_METRICS"); v != "" {
		b := truthy(v)
		c.HostMetrics = &b
	}
	if v := getenv("LUMEN_AGENT_LOG_PATHS"); v != "" {
		var paths []string
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				paths = append(paths, p)
			}
		}
		c.Logs = append(c.Logs, LogSource{Paths: paths, Service: "system-logs", Format: "text"})
	}
	if truthy(getenv("LUMEN_AGENT_DOCKER_LOGS")) {
		c.Logs = append(c.Logs, LogSource{Paths: []string{"/var/lib/docker/containers/*/*-json.log"}, Service: "docker", Format: "docker"})
	}
	if u := getenv("LUMEN_AGENT_NEXTCLOUD_URL"); u != "" {
		c.Nextcloud = append(c.Nextcloud, NextcloudTarget{
			URL: u, Service: getenv("LUMEN_AGENT_NEXTCLOUD_SERVICE"), Token: getenv("LUMEN_AGENT_NEXTCLOUD_TOKEN"),
			Username: getenv("LUMEN_AGENT_NEXTCLOUD_USER"), Password: getenv("LUMEN_AGENT_NEXTCLOUD_PASSWORD"),
			LogPath: getenv("LUMEN_AGENT_NEXTCLOUD_LOG"),
		})
	}
	if c.URL == "" {
		c.URL = "http://localhost:4318"
	}
	if c.HostService == "" {
		c.HostService = "host"
	}
	if c.IntervalSeconds < 1 {
		c.IntervalSeconds = 15
	}
	for i := range c.Nextcloud {
		if c.Nextcloud[i].Service == "" {
			c.Nextcloud[i].Service = "nextcloud"
		}
	}
	return c, nil
}
