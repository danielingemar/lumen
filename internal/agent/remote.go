package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RemoteInstance and RemoteConfig mirror what the server sends to GET /api/v1/agent/config.
type RemoteInstance struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Token    string `json:"token"`
	Username string `json:"username"`
	Password string `json:"password"`
	LogPath  string `json:"log_path"`
}

type RemoteConfig struct {
	Revision      string           `json:"revision"`
	LogPaths      []string         `json:"log_paths"`
	DockerLogs    bool             `json:"docker_logs"`
	Systemd       bool             `json:"systemd"`
	Containers    bool             `json:"containers"`
	WatchServices []string         `json:"watch_services"`
	Instances     []RemoteInstance `json:"instances"`
	UpdateTo      string           `json:"update_to,omitempty"` // set when someone asked in the UI for this agent to be updated
}

// ErrRemoteUnsupported means the server has no remote configuration (an older server): keep the local config.
var ErrRemoteUnsupported = fmt.Errorf("server does not support remote configuration")

// FetchRemote asks the server what this host should do.
func FetchRemote(ctx context.Context, baseURL, key, host string) (RemoteConfig, error) {
	var rc RemoteConfig
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "GET", strings.TrimRight(baseURL, "/")+"/api/v1/agent/config?host="+url.QueryEscape(host)+"&version="+url.QueryEscape(Version), nil)
	if err != nil {
		return rc, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return rc, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return rc, ErrRemoteUnsupported
	}
	if resp.StatusCode != 200 {
		return rc, fmt.Errorf("config request: HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rc); err != nil {
		return rc, fmt.Errorf("config response: %w", err)
	}
	return rc, nil
}

// Merge combines the local config (file, flags, env) with what the server says. Remote log paths, Docker logs and
// instances are added to the local ones; an instance configured in the UI replaces a local one with the same URL.
func (c Config) Merge(r RemoteConfig) Config {
	out := c
	out.Logs = append([]LogSource(nil), c.Logs...)
	out.Nextcloud = nil
	out.RejectedLogPaths = nil
	allow := func(p string) bool {
		if LogPathAllowed(p, c.AllowedLogDirs) {
			return true
		}
		out.RejectedLogPaths = append(out.RejectedLogPaths, p)
		return false
	}
	var paths []string
	for _, p := range r.LogPaths {
		if allow(p) {
			paths = append(paths, p)
		}
	}
	if len(paths) > 0 {
		out.Logs = append(out.Logs, LogSource{Paths: paths, Service: "system-logs", Format: "text"})
	}
	if r.DockerLogs {
		out.Logs = append(out.Logs, LogSource{Paths: []string{"/var/lib/docker/containers/*/*-json.log"}, Service: "docker", Format: "docker"})
	}
	remoteURLs := map[string]bool{}
	for _, i := range r.Instances {
		remoteURLs[strings.TrimRight(i.URL, "/")] = true
	}
	for _, n := range c.Nextcloud {
		if !remoteURLs[strings.TrimRight(n.URL, "/")] {
			out.Nextcloud = append(out.Nextcloud, n)
		}
	}
	for _, i := range r.Instances {
		lp := i.LogPath
		if lp != "" && !allow(lp) {
			lp = ""
		}
		out.Nextcloud = append(out.Nextcloud, NextcloudTarget{URL: i.URL, Service: i.Name, Token: i.Token, Username: i.Username, Password: i.Password, LogPath: lp})
	}
	sys, ctr := r.Systemd, r.Containers
	out.Systemd, out.Containers = &sys, &ctr
	out.WatchServices = r.WatchServices
	return out
}
