package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const systemctlOut = `  accounts-daemon.service     loaded    active   running Accounts Service
  auditd.service              loaded    active   running Security Auditing Service
● postfix.service             loaded    failed   failed  Postfix Mail Transport Agent
  cups.service                loaded    inactive dead    CUPS Scheduler
  getty@tty1.service          loaded    active   running Getty on tty1
  fstrim.service              loaded    inactive dead    Discard unused blocks
  sshd.service                loaded    active   running OpenSSH server daemon
  ghost.service               not-found inactive dead    ghost.service
  nginx.service               loaded    inactive dead    nginx
`

func TestParseSystemd(t *testing.T) {
	pts := ParseSystemd(systemctlOut, []string{"nginx.service", "mysqld", "ghost"}, 42)
	got := map[string]Point{}
	for _, p := range pts {
		if p.Name != "system_service_up" || p.TimeNs != 42 {
			t.Fatalf("bad point %+v", p)
		}
		got[p.Attrs["service"]] = p
	}
	want := map[string]float64{"accounts-daemon": 1, "auditd": 1, "sshd": 1, "postfix": 0, "nginx": 0, "mysqld": 0, "ghost": 0}
	for n, v := range want {
		p, ok := got[n]
		if !ok || p.Value != v {
			t.Errorf("%s: want up=%v, got %+v (present=%v)", n, v, p, ok)
		}
	}
	if got["postfix"].Attrs["state"] != "failed" {
		t.Error("failed units (with the ● marker) must be reported with their state")
	}
	if got["mysqld"].Attrs["state"] != "not-found" {
		t.Error("a watched service that is not installed or not loaded is down")
	}
	for _, noise := range []string{"cups", "fstrim", "getty@tty1"} {
		if _, ok := got[noise]; ok {
			t.Errorf("%s: inactive or template units must not be reported unless watched", noise)
		}
	}
	if len(ParseSystemd("garbage\n\n", nil, 1)) != 0 {
		t.Fatal("garbage must be ignored")
	}
}

func TestCollectContainersOverUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("no unix sockets here")
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/json" || r.URL.Query().Get("all") != "1" {
			http.Error(w, "unexpected "+r.URL.String(), 400)
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{
			{"Names": []string{"/nextcloud-app"}, "Image": "nextcloud:29", "State": "running"},
			{"Names": []string{"/old-db"}, "Image": "mariadb:11", "State": "exited"},
			{"Names": []string{}, "Image": "x", "State": "running"},
		})
	}))
	srv.Listener = l
	srv.Start()
	defer srv.Close()
	pts, err := CollectContainers(context.Background(), sock, 7)
	if err != nil || len(pts) != 2 {
		t.Fatalf("%v %+v", err, pts)
	}
	if pts[0].Attrs["container"] != "nextcloud-app" || pts[0].Value != 1 || pts[0].Attrs["image"] != "nextcloud:29" || pts[1].Value != 0 || pts[1].Attrs["state"] != "exited" {
		t.Fatalf("%+v", pts)
	}
	if _, err := CollectContainers(context.Background(), filepath.Join(t.TempDir(), "nope.sock"), 1); err != ErrNoDocker {
		t.Fatalf("missing socket must be ErrNoDocker (quiet), got %v", err)
	}
	if err := os.Chmod(sock, 0); err == nil && os.Getuid() != 0 {
		if _, err := CollectContainers(context.Background(), sock, 1); err == nil {
			t.Fatal("an unreadable socket must be an error that says how to fix it")
		} else if !contains(err.Error(), "docker group") {
			t.Fatalf("error must explain the fix: %v", err)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestRemoteConfigFetchAndMerge(t *testing.T) {
	var gotAuth, gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotHost = r.Header.Get("Authorization"), r.URL.Query().Get("host")
		json.NewEncoder(w).Encode(RemoteConfig{Revision: "abc", LogPaths: []string{"/var/log/nginx/*.log"}, DockerLogs: true, Systemd: true, Containers: false,
			WatchServices: []string{"nginx"}, Instances: []RemoteInstance{{Name: "ks", URL: "https://cloud.example.com", Token: "t", LogPath: "/x.log"}}})
	}))
	defer srv.Close()
	rc, err := FetchRemote(context.Background(), srv.URL, "KEY", "web 1")
	if err != nil || rc.Revision != "abc" || gotAuth != "Bearer KEY" || gotHost != "web 1" {
		t.Fatalf("%v %+v auth=%q host=%q", err, rc, gotAuth, gotHost)
	}
	local := Config{Logs: []LogSource{{Paths: []string{"/local.log"}, Service: "local", Format: "text"}},
		Nextcloud: []NextcloudTarget{{URL: "https://cloud.example.com/", Service: "old-name", Token: "stale"}, {URL: "https://other.example.com", Service: "other"}}}
	m := local.Merge(rc)
	if len(m.Logs) != 3 || m.Logs[1].Paths[0] != "/var/log/nginx/*.log" || m.Logs[2].Format != "docker" {
		t.Fatalf("logs: %+v", m.Logs)
	}
	if len(m.Nextcloud) != 2 || m.Nextcloud[0].Service != "other" || m.Nextcloud[1].Service != "ks" || m.Nextcloud[1].Token != "t" {
		t.Fatalf("an instance from the UI replaces a local one with the same URL: %+v", m.Nextcloud)
	}
	if !m.SystemdOn() || m.ContainersOn() || len(m.WatchServices) != 1 {
		t.Fatalf("flags: %+v", m)
	}
	if len(local.Logs) != 1 || len(local.Nextcloud) != 2 {
		t.Fatal("Merge must not modify the local config")
	}
	// old servers and broken servers
	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	if _, err := FetchRemote(context.Background(), old.URL, "k", "h"); err != ErrRemoteUnsupported {
		t.Fatalf("404 = older server without remote config: %v", err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 401) }))
	defer bad.Close()
	if _, err := FetchRemote(context.Background(), bad.URL, "k", "h"); err == nil || err == ErrRemoteUnsupported {
		t.Fatalf("a rejected key must be an error: %v", err)
	}
	if !(Config{}).SystemdOn() || !(Config{}).ContainersOn() {
		t.Fatal("services and containers are on by default")
	}
}

func TestLogPathAllowList(t *testing.T) {
	ok := []string{"/var/log/syslog", "/var/log/nginx/*.log", "/var/log/ng*", "/var/lib/docker/volumes/nc_data/_data/data/nextcloud.log", "/srv/app/logs/app.log", "/opt/x/y.log"}
	bad := []string{"/etc/shadow", "/root/.ssh/id_rsa", "/home/bob/.ssh/*", "/var/log/../../etc/shadow", "/var/log/*/../../../etc/shadow", "/var/logs/x", "/var/lo*", "/var/*/../../etc/passwd", "relative.log", "var/log/x", "/", "/*", "/var/lib/docker/*", "/var/lib/docker/overlay2/x/diff/etc/shadow"}
	for _, p := range ok {
		if !LogPathAllowed(p, nil) {
			t.Errorf("%s should be allowed by default", p)
		}
	}
	for _, p := range bad {
		if LogPathAllowed(p, nil) {
			t.Errorf("%s must be refused by default", p)
		}
	}
	if !LogPathAllowed("/etc/shadow", []string{"*"}) || !LogPathAllowed("/home/app/logs/a.log", []string{"/home/app"}) || LogPathAllowed("/home/other/a.log", []string{"/home/app"}) {
		t.Fatal("the local allowed_log_dirs setting overrides the default")
	}
	// merge: refused paths are dropped from the config and reported
	rc := RemoteConfig{LogPaths: []string{"/var/log/ok.log", "/etc/shadow"}, Instances: []RemoteInstance{{Name: "a", URL: "https://a.example.com", LogPath: "/root/secret.log"}}}
	m := Config{}.Merge(rc)
	if len(m.Logs) != 1 || len(m.Logs[0].Paths) != 1 || m.Logs[0].Paths[0] != "/var/log/ok.log" || m.Nextcloud[0].LogPath != "" || len(m.RejectedLogPaths) != 2 {
		t.Fatalf("%+v", m)
	}
	// ...but paths in the local config file are trusted
	local := Config{Logs: []LogSource{{Paths: []string{"/etc/special.log"}, Service: "x", Format: "text"}}}.Merge(RemoteConfig{})
	if len(local.Logs) != 1 || len(local.RejectedLogPaths) != 0 {
		t.Fatal("local paths must stay")
	}
}
