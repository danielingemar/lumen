package agent

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
	"sort"
	"strconv"
	"strings"
	"time"
)

// Redis monitoring. The agent finds Redis running in Docker by itself (an image called redis, valkey or keydb) and also
// checks addresses it was told about. A Redis without a password just works. A Redis with a password is reported as
// "needs a password" until one is given in the Lumen UI; the server hands it to this agent, which uses it for exactly that
// Redis. The client below speaks the little of the Redis protocol that is needed (AUTH and INFO), so nothing is installed.

// RedisTarget is one Redis to check. Name is what it is called in Lumen (the container name when it is found by itself).
type RedisTarget struct {
	Name      string `json:"name"`
	Container string `json:"container,omitempty"` // the Docker container this is (matches the discovered one)
	Addr      string `json:"addr,omitempty"`      // host:port, for a Redis that is not in Docker or cannot be found by itself
	Username  string `json:"username,omitempty"`  // Redis 6 ACL user; empty for the classic single password
	Password  string `json:"password,omitempty"`
}

// ErrRedisAuth means the Redis wants a password and none (or a wrong one) was given.
var ErrRedisAuth = errors.New("redis requires a (correct) password")

// ErrRedisNoSpace is used when a reply is bigger than we are willing to read.
var errRedisBig = errors.New("redis reply too large")

type redisConn struct {
	c net.Conn
	r *bufio.Reader
}

func redisDial(ctx context.Context, addr string) (*redisConn, error) {
	d := net.Dialer{Timeout: 3 * time.Second}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return &redisConn{c: c, r: bufio.NewReaderSize(c, 64<<10)}, nil
}

func (rc *redisConn) close() { rc.c.Close() }

// do sends one command and returns its reply (simple string, bulk string or error text as a Go error).
func (rc *redisConn) do(args ...string) (string, error) {
	rc.c.SetDeadline(time.Now().Add(5 * time.Second))
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := io.WriteString(rc.c, b.String()); err != nil {
		return "", err
	}
	line, err := rc.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errors.New("empty reply from redis")
	}
	switch line[0] {
	case '+', ':':
		return line[1:], nil
	case '-':
		return "", errors.New(line[1:])
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return "", errors.New("not a redis reply")
		}
		if n < 0 {
			return "", nil
		}
		if n > 4<<20 {
			return "", errRedisBig
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(rc.r, buf); err != nil {
			return "", err
		}
		return string(buf[:n]), nil
	}
	return "", errors.New("not a redis reply (is this port really Redis?)")
}

func isAuthErr(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToUpper(err.Error())
	return strings.HasPrefix(m, "NOAUTH") || strings.HasPrefix(m, "WRONGPASS") || strings.HasPrefix(m, "ERR INVALID PASSWORD") ||
		strings.Contains(m, "AUTHENTICATION REQUIRED") || strings.HasPrefix(m, "ERR AUTH") || strings.HasPrefix(m, "NOPERM")
}

// RedisInfo connects, logs in when a password is given, and returns the parsed INFO. A Redis that wants a password gives
// ErrRedisAuth. A password given to a Redis that has none is fine (it is simply not needed).
func RedisInfo(ctx context.Context, addr, user, pass string) (map[string]string, error) {
	rc, err := redisDial(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer rc.close()
	if pass != "" {
		args := []string{"AUTH", pass}
		if user != "" {
			args = []string{"AUTH", user, pass}
		}
		if _, err := rc.do(args...); err != nil {
			switch {
			case strings.Contains(strings.ToLower(err.Error()), "without any password configured"), strings.Contains(strings.ToLower(err.Error()), "no password is set"):
				// the Redis has no password: carry on without
			case isAuthErr(err):
				return nil, ErrRedisAuth
			default:
				return nil, err
			}
		}
	}
	out, err := rc.do("INFO")
	if err != nil {
		if isAuthErr(err) {
			return nil, ErrRedisAuth
		}
		return nil, err
	}
	return ParseRedisInfo(out), nil
}

// ParseRedisInfo turns the text of INFO into key/value pairs (section headers and blank lines are skipped).
func ParseRedisInfo(s string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || l[0] == '#' {
			continue
		}
		if k, v, ok := strings.Cut(l, ":"); ok {
			m[k] = v
		}
	}
	return m
}

// redisKeys adds up the keys of all databases from lines like "db0:keys=12,expires=1,avg_ttl=0".
func redisKeys(m map[string]string) float64 {
	var n float64
	for k, v := range m {
		if len(k) > 2 && strings.HasPrefix(k, "db") && k[2] >= '0' && k[2] <= '9' {
			for _, f := range strings.Split(v, ",") {
				if a, b, ok := strings.Cut(f, "="); ok && a == "keys" {
					x, _ := strconv.ParseFloat(b, 64)
					n += x
				}
			}
		}
	}
	return n
}

// RedisPoints turns an INFO into metrics. Counters are "sum" (they only grow, until Redis restarts).
func RedisPoints(m map[string]string, attrs map[string]string, now int64) []Point {
	var pts []Point
	g := func(name, key, typ string) {
		v, ok := m[key]
		if !ok {
			return
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return
		}
		pts = append(pts, Point{Service: "host", Name: name, Type: typ, Value: f, TimeNs: now, Attrs: attrs})
	}
	g("redis_memory_used_bytes", "used_memory", "gauge")
	g("redis_memory_max_bytes", "maxmemory", "gauge")
	g("redis_clients", "connected_clients", "gauge")
	g("redis_blocked_clients", "blocked_clients", "gauge")
	g("redis_uptime_seconds", "uptime_in_seconds", "gauge")
	g("redis_connected_replicas", "connected_slaves", "gauge")
	g("redis_evicted_keys_total", "evicted_keys", "sum")
	g("redis_keyspace_hits_total", "keyspace_hits", "sum")
	g("redis_keyspace_misses_total", "keyspace_misses", "sum")
	g("redis_commands_total", "total_commands_processed", "sum")
	g("redis_rejected_connections_total", "rejected_connections", "sum")
	pts = append(pts, Point{Service: "host", Name: "redis_keys", Type: "gauge", Value: redisKeys(m), TimeNs: now, Attrs: attrs})
	if used, ok1 := m["used_memory"]; ok1 {
		u, _ := strconv.ParseFloat(strings.TrimSpace(used), 64)
		if mx, _ := strconv.ParseFloat(strings.TrimSpace(m["maxmemory"]), 64); mx > 0 { // only a Redis with a limit has a "how full"
			pts = append(pts, Point{Service: "host", Name: "redis_memory_utilization", Type: "gauge", Value: u / mx, TimeNs: now, Attrs: attrs})
		}
	}
	master := 0.0
	if m["role"] == "master" {
		master = 1
	}
	pts = append(pts, Point{Service: "host", Name: "redis_master", Type: "gauge", Value: master, TimeNs: now, Attrs: attrs})
	if v, ok := m["rdb_last_bgsave_status"]; ok {
		ok := 0.0
		if v == "ok" {
			ok = 1
		}
		pts = append(pts, Point{Service: "host", Name: "redis_save_ok", Type: "gauge", Value: ok, TimeNs: now, Attrs: attrs})
	}
	return pts
}

// redisImage says whether a Docker image is a Redis server (and not a tool around Redis, such as an exporter or a web UI).
func redisImage(image string) bool {
	img := strings.ToLower(image)
	if i := strings.LastIndex(img, "/"); i >= 0 {
		img = img[i+1:]
	}
	img = strings.SplitN(img, ":", 2)[0]
	img = strings.SplitN(img, "@", 2)[0]
	for _, bad := range []string{"exporter", "insight", "commander", "sentinel", "cluster-proxy", "stack-server-gui", "ui", "admin", "webui"} {
		if strings.Contains(img, bad) {
			return false
		}
	}
	for _, good := range []string{"redis", "valkey", "keydb", "dragonfly", "redict"} {
		if strings.Contains(img, good) {
			return true
		}
	}
	return false
}

type dockerRedisContainer struct {
	Names  []string
	Image  string
	State  string
	Labels map[string]string
	Ports  []struct {
		PrivatePort int
		PublicPort  int
		IP          string
		Type        string
	}
	NetworkSettings struct {
		Networks map[string]struct{ IPAddress string }
	}
}

// DiscoveredRedis is a Redis found in a Docker container. Addrs are the addresses to try, in order: the container's own
// address on its Docker networks (works when the agent runs on the host or shares a network) and the published port.
type DiscoveredRedis struct {
	Container string
	Image     string
	Addrs     []string
}

// DiscoverRedis lists the running containers that are Redis servers.
func DiscoverRedis(ctx context.Context, socket string) ([]DiscoveredRedis, error) {
	if socket == "" {
		socket = dockerSocket()
	}
	if _, err := os.Stat(socket); err != nil {
		return nil, ErrNoDocker
	}
	cl := dockerClient(socket)
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://docker/containers/json", nil)
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
	var cs []dockerRedisContainer
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 8<<20)).Decode(&cs); err != nil {
		return nil, err
	}
	return redisFromContainers(cs), nil
}

func redisFromContainers(cs []dockerRedisContainer) []DiscoveredRedis {
	var out []DiscoveredRedis
	for _, c := range cs {
		if c.State != "running" || len(c.Names) == 0 || !redisImage(c.Image) || c.Labels["lumen.redis"] == "false" {
			continue
		}
		name := strings.TrimPrefix(c.Names[0], "/")
		private, public := 6379, 0
		for _, p := range c.Ports {
			if p.PrivatePort == 6379 {
				private = 6379
				if p.PublicPort != 0 && public == 0 {
					public = p.PublicPort
				}
			}
		}
		if v := c.Labels["lumen.redis.port"]; v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
				private = n
			}
		}
		d := DiscoveredRedis{Container: name, Image: c.Image}
		var nets []string
		for n := range c.NetworkSettings.Networks {
			nets = append(nets, n)
		}
		sort.Strings(nets)
		for _, n := range nets {
			if ip := c.NetworkSettings.Networks[n].IPAddress; ip != "" {
				d.Addrs = append(d.Addrs, net.JoinHostPort(ip, strconv.Itoa(private)))
			}
		}
		if public != 0 {
			d.Addrs = append(d.Addrs, net.JoinHostPort("127.0.0.1", strconv.Itoa(public)))
		}
		out = append(out, d)
	}
	return out
}

// CollectRedis checks every Redis it can find (in Docker) plus the ones in targets, and returns their metrics. passwords
// are matched to Docker-found Redis by container name; a target with an Addr is checked at that address. defaultPass is
// tried for a Redis without its own password (so one shared password for all of them works too).
func CollectRedis(ctx context.Context, socket string, targets []RedisTarget, defaultPass string, now int64) ([]Point, error) {
	found, derr := DiscoverRedis(ctx, socket)
	if errors.Is(derr, ErrNoDocker) {
		derr = nil
	}
	type job struct {
		name, container string
		addrs           []string
		user, pass      string
	}
	var jobs []job
	byContainer := map[string]RedisTarget{}
	for _, t := range targets {
		if t.Container != "" {
			byContainer[t.Container] = t
		}
	}
	seen := map[string]bool{}
	for _, d := range found {
		t := byContainer[d.Container]
		name := t.Name
		if name == "" {
			name = d.Container
		}
		pass := t.Password
		if pass == "" {
			pass = defaultPass
		}
		jobs = append(jobs, job{name, d.Container, d.Addrs, t.Username, pass})
		seen[d.Container] = true
	}
	for _, t := range targets {
		if t.Addr == "" || (t.Container != "" && seen[t.Container]) {
			continue
		}
		name := t.Name
		if name == "" {
			name = t.Addr
		}
		pass := t.Password
		if pass == "" {
			pass = defaultPass
		}
		jobs = append(jobs, job{name, t.Container, []string{t.Addr}, t.Username, pass})
	}
	// a record for a container that has an explicit address as well: that address is tried first
	for i := range jobs {
		if t, ok := byContainer[jobs[i].container]; ok && t.Addr != "" {
			jobs[i].addrs = append([]string{t.Addr}, jobs[i].addrs...)
		}
	}
	if len(jobs) > 50 {
		jobs = jobs[:50]
	}
	var pts []Point
	for _, j := range jobs {
		attrs := map[string]string{"redis": j.name}
		if j.container != "" {
			attrs["container"] = j.container
		}
		var info map[string]string
		var err error
		for _, a := range j.addrs {
			info, err = RedisInfo(ctx, a, j.user, j.pass)
			if err == nil || errors.Is(err, ErrRedisAuth) {
				attrs["addr"] = a
				break
			}
		}
		if len(j.addrs) == 0 {
			err = errors.New("no address to reach it on")
		}
		up, auth := 1.0, 0.0
		if err != nil {
			up = 0
			if errors.Is(err, ErrRedisAuth) {
				auth = 1
				if j.pass != "" {
					attrs["reason"] = "the password is wrong"
				} else {
					attrs["reason"] = "needs a password"
				}
			} else {
				attrs["reason"] = shortErr(err)
			}
		}
		base := map[string]string{"redis": j.name}
		pts = append(pts,
			Point{Service: "host", Name: "redis_up", Type: "gauge", Value: up, TimeNs: now, Attrs: base},
			Point{Service: "host", Name: "redis_auth_required", Type: "gauge", Value: auth, TimeNs: now, Attrs: base})
		ia := map[string]string{"redis": j.name, "container": j.container, "addr": attrs["addr"], "reason": attrs["reason"]}
		if err == nil {
			ia["version"], ia["mode"], ia["role"] = info["redis_version"], info["redis_mode"], info["role"]
			if v := info["valkey_version"]; v != "" {
				ia["version"] = v
			}
			pts = append(pts, RedisPoints(info, base, now)...)
		}
		pts = append(pts, Point{Service: "host", Name: "redis_info", Type: "gauge", Value: 1, TimeNs: now, Attrs: ia})
	}
	return pts, derr
}

func shortErr(err error) string {
	m := err.Error()
	if i := strings.LastIndex(m, ": "); i >= 0 && strings.Contains(m, "dial tcp") {
		m = m[i+2:]
	}
	if len(m) > 120 {
		m = m[:120]
	}
	return m
}
