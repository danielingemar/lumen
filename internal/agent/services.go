package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
)

const maxServicePoints = 500

// ParseSystemd reads the output of
//
//	systemctl list-units --type=service --all --no-legend --no-pager --plain
//
// and returns a "system_service_up" gauge for every unit that is active or failed, plus every unit in watch
// (reported as down when it is stopped or does not exist). A unit is up (1) only when its state is "active".
func ParseSystemd(out string, watch []string, now int64) []Point {
	want := map[string]bool{}
	for _, w := range watch {
		w = strings.TrimSuffix(strings.TrimSpace(w), ".service")
		if w != "" {
			want[w] = true
		}
	}
	seen := map[string]bool{}
	var pts []Point
	mk := func(name, state string, up float64) Point {
		return Point{Service: "host", Name: "system_service_up", Type: "gauge", Value: up, TimeNs: now,
			Attrs: map[string]string{"service": name, "state": state}}
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) > 0 && (f[0] == "●" || f[0] == "*" || f[0] == "○" || f[0] == "×") { // marker systemd puts before failed units
			f = f[1:]
		}
		if len(f) < 4 || !strings.HasSuffix(f[0], ".service") {
			continue
		}
		name, load, active, sub := strings.TrimSuffix(f[0], ".service"), f[1], f[2], f[3]
		if strings.Contains(name, "@") && !want[name] { // template instances such as getty@tty1 are noise
			continue
		}
		seen[name] = true
		if load == "not-found" && !want[name] {
			continue
		}
		if active == "active" || active == "failed" || want[name] {
			up := 0.0
			if active == "active" {
				up = 1
			}
			pts = append(pts, mk(name, sub, up))
		}
	}
	var missing []string
	for w := range want {
		if !seen[w] {
			missing = append(missing, w)
		}
	}
	sort.Strings(missing)
	for _, w := range missing {
		pts = append(pts, mk(w, "not-found", 0))
	}
	if len(pts) > maxServicePoints {
		pts = pts[:maxServicePoints]
	}
	return pts
}

// CollectSystemd lists systemd services. It returns nil on systems without systemd (other OS, containers).
func CollectSystemd(ctx context.Context, watch []string, now int64) ([]Point, error) {
	if runtime.GOOS != "linux" {
		return nil, nil
	}
	path, err := exec.LookPath("systemctl")
	if err != nil {
		return nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, "list-units", "--type=service", "--all", "--no-legend", "--no-pager", "--plain").Output()
	if err != nil {
		return nil, fmt.Errorf("systemctl: %w", err)
	}
	return ParseSystemd(string(out), watch, now), nil
}

// ---- Docker containers (Engine API over the local socket; no docker CLI needed) ----

type dockerContainer struct {
	Names []string
	Image string
	State string
}

func dockerSocket() string {
	if h := os.Getenv("DOCKER_HOST"); strings.HasPrefix(h, "unix://") {
		return strings.TrimPrefix(h, "unix://")
	}
	return "/var/run/docker.sock"
}

// ErrNoDocker means Docker is not installed on this machine (not an error worth reporting).
var ErrNoDocker = errors.New("docker is not installed")

// CollectContainers reports a "container_up" gauge per container: 1 when running, 0 otherwise.
func CollectContainers(ctx context.Context, socket string, now int64) ([]Point, error) {
	if socket == "" {
		socket = dockerSocket()
	}
	if _, err := os.Stat(socket); err != nil {
		return nil, ErrNoDocker
	}
	cl := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://docker/containers/json?all=1", nil)
	resp, err := cl.Do(req)
	if err != nil {
		if errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "permission denied") {
			return nil, fmt.Errorf("no access to the Docker socket %s: add the agent's user to the docker group (re-run the installer with --docker)", socket)
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("docker API: %s", resp.Status)
	}
	var cs []dockerContainer
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 8<<20)).Decode(&cs); err != nil {
		return nil, err
	}
	var pts []Point
	for _, c := range cs {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		if name == "" {
			continue
		}
		up := 0.0
		if c.State == "running" {
			up = 1
		}
		pts = append(pts, Point{Service: "host", Name: "container_up", Type: "gauge", Value: up, TimeNs: now,
			Attrs: map[string]string{"container": name, "image": c.Image, "state": c.State}})
		if len(pts) >= maxServicePoints {
			break
		}
	}
	return pts, nil
}
