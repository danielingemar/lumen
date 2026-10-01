package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Shape follows the Nextcloud "serverinfo" app OCS response. Verify against your own instance.
const serverinfoJSON = `{"ocs":{"meta":{"status":"ok","statuscode":200},"data":{
 "nextcloud":{"system":{"version":"33.0.0.1","freespace":123456789,"apps":{"num_installed":42,"num_updates_available":3}},
  "storage":{"num_users":17,"num_files":"98765","num_storages":30,"num_storages_local":1,"num_storages_home":17,"num_storages_other":12},
  "shares":{"num_shares":11,"num_shares_user":4,"num_shares_groups":2,"num_shares_link":5,"num_shares_link_no_password":1,"num_fed_shares_sent":0,"num_fed_shares_received":0}},
 "server":{"webserver":"Apache","php":{"version":"8.3.1","memory_limit":536870912,"max_execution_time":3600,
   "opcache":{"opcache_statistics":{"opcache_hit_rate":99.5},"memory_usage":{"used_memory":50000000,"current_wasted_percentage":0.4}}},
  "database":{"type":"pgsql","version":"18.0","size":73400320}},
 "activeUsers":{"last5minutes":2,"last1hour":5,"last24hours":9}}}}`

func fakeNextcloud(t *testing.T, token string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status.php":
			w.Write([]byte(`{"installed":true,"maintenance":false,"needsDbUpgrade":false,"version":"33.0.0.1","versionstring":"33.0.0"}`))
		case "/ocs/v2.php/apps/serverinfo/api/v1/info":
			if r.Header.Get("NC-Token") != token || r.Header.Get("OCS-APIRequest") != "true" {
				w.WriteHeader(401)
				return
			}
			w.Write([]byte(serverinfoJSON))
		default:
			w.WriteHeader(404)
		}
	}))
}

func find(pts []Point, name string, kv ...string) (Point, bool) {
outer:
	for _, p := range pts {
		if p.Name != name {
			continue
		}
		for i := 0; i < len(kv); i += 2 {
			if p.Attrs[kv[i]] != kv[i+1] {
				continue outer
			}
		}
		return p, true
	}
	return Point{}, false
}

func TestNextcloudCollect(t *testing.T) {
	ts := fakeNextcloud(t, "s3cret")
	defer ts.Close()
	pts, err := CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL, Service: "nc-ks", Token: "s3cret"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		kv   []string
		want float64
	}{
		{"nextcloud_up", nil, 1}, {"nextcloud_maintenance_mode", nil, 0},
		{"nextcloud_users", nil, 17}, {"nextcloud_files", nil, 98765}, // numeric string handled
		{"nextcloud_active_users", []string{"period", "24h"}, 9}, {"nextcloud_shares", []string{"type", "link"}, 5},
		{"nextcloud_storages", []string{"type", "home"}, 17}, {"nextcloud_apps_updates_available", nil, 3},
		{"nextcloud_database_size_bytes", nil, 73400320}, {"nextcloud_php_opcache_hit_rate", nil, 99.5},
		{"nextcloud_php_memory_limit_bytes", nil, 536870912},
	}
	for _, c := range checks {
		p, ok := find(pts, c.name, c.kv...)
		if !ok || p.Value != c.want || p.Service != "nc-ks" {
			t.Errorf("%s%v: got %+v ok=%v, want %v", c.name, c.kv, p, ok, c.want)
		}
	}
	info, ok := find(pts, "nextcloud_info")
	if !ok || info.Attrs["version"] != "33.0.0.1" || info.Attrs["db_type"] != "pgsql" || info.Attrs["php_version"] != "8.3.1" {
		t.Errorf("info metric: %+v", info)
	}
}

func TestNextcloudBadTokenStillReportsUp(t *testing.T) {
	ts := fakeNextcloud(t, "right")
	defer ts.Close()
	pts, err := CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL, Token: "wrong"}, 1)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want a 401 error with a hint, got %v", err)
	}
	if p, ok := find(pts, "nextcloud_up"); !ok || p.Value != 1 {
		t.Fatalf("availability must still be reported: %+v", pts)
	}
}

func TestNextcloudDownAndAvailabilityOnly(t *testing.T) {
	pts, err := CollectNextcloud(context.Background(), NextcloudTarget{URL: "http://127.0.0.1:1"}, 1)
	if err == nil {
		t.Fatal("expected error")
	}
	if p, ok := find(pts, "nextcloud_up"); !ok || p.Value != 0 {
		t.Fatalf("down instance must report nextcloud_up=0: %+v", pts)
	}
	ts := fakeNextcloud(t, "x")
	defer ts.Close()
	pts, err = CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL}, 1) // no credentials
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := find(pts, "nextcloud_users"); ok {
		t.Fatal("no serverinfo metrics expected without credentials")
	}
	if _, ok := find(pts, "nextcloud_info"); !ok {
		t.Fatal("info from status.php expected")
	}
}

func TestNextcloudLogFormat(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "nextcloud.log")
	os.WriteFile(f, nil, 0o644)
	tl := &Tailer{Patterns: []string{f}, Service: "nc", Format: "nextcloud"}
	tl.Poll()
	fh, _ := os.OpenFile(f, os.O_APPEND|os.O_WRONLY, 0o644)
	fh.WriteString(`{"reqId":"abc","level":3,"time":"2026-09-30T10:11:12+00:00","remoteAddr":"1.2.3.4","user":"alice","app":"files","method":"PUT","url":"/remote.php/dav/x?token=SECRET","message":"Disk full"}` + "\n")
	fh.WriteString("not json at all\n")
	got := tl.Poll()
	if len(got) != 2 {
		t.Fatalf("want 2 lines, got %+v", got)
	}
	g := got[0]
	if g.Body != "Disk full" || g.Severity != "ERROR" || g.Attrs["nextcloud.user"] != "alice" || g.Attrs["nextcloud.app"] != "files" {
		t.Fatalf("parsed: %+v", g)
	}
	for k, v := range g.Attrs {
		if strings.Contains(v, "SECRET") || strings.Contains(k, "url") {
			t.Fatalf("request URL must not be copied (may contain tokens): %s=%s", k, v)
		}
	}
	if got[1].Body != "not json at all" {
		t.Fatalf("non-JSON line must be kept as text: %+v", got[1])
	}
}

func TestLoadConfigEnv(t *testing.T) {
	env := map[string]string{
		"LUMEN_AGENT_URL": "http://l:4318", "LUMEN_AGENT_API_KEY": "k", "LUMEN_AGENT_DOCKER_LOGS": "1",
		"LUMEN_AGENT_HOST_METRICS": "false", "LUMEN_AGENT_NEXTCLOUD_URL": "https://c.example", "LUMEN_AGENT_NEXTCLOUD_TOKEN": "t",
		"LUMEN_AGENT_LOG_PATHS": "/a/*.log, /b/*.log",
	}
	c, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"), func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.URL != "http://l:4318" || c.APIKey != "k" || c.HostOn() || !c.SelfOn() || c.IntervalSeconds != 15 {
		t.Fatalf("basics: %+v", c)
	}
	if len(c.Logs) != 2 || c.Logs[0].Paths[1] != "/b/*.log" || c.Logs[1].Format != "docker" {
		t.Fatalf("logs: %+v", c.Logs)
	}
	if len(c.Nextcloud) != 1 || c.Nextcloud[0].Service != "nextcloud" || c.Nextcloud[0].Token != "t" {
		t.Fatalf("nextcloud: %+v", c.Nextcloud)
	}
	// defaults with nothing set: host metrics on
	c2, _ := LoadConfig(filepath.Join(t.TempDir(), "missing.json"), func(string) string { return "" })
	if !c2.HostOn() {
		t.Fatal("host metrics must default to on")
	}
	// a broken file is an error, not silently ignored
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte("{nope"), 0o644)
	if _, err := LoadConfig(bad, func(string) string { return "" }); err == nil {
		t.Fatal("invalid JSON config must fail")
	}
}
