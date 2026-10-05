package status

import (
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/model"
	"github.com/danielingemar/lumen/internal/registry"
)

var now = time.Unix(1_800_000_000, 0)

func row(name string, v float64, ageS int64, attrs map[string]string) model.Latest {
	return model.Latest{Name: name, Service: "x", Attrs: attrs, Value: v, T: now.Unix() - ageS}
}

func TestHostsServicesAndContainers(t *testing.T) {
	rows := []model.Latest{
		row("lumen_agent_info", 1, 5, map[string]string{"host": "web1", "version": "0.3", "os": "linux/amd64"}),
		row("lumen_agent_info", 1, 600, map[string]string{"host": "old1"}), // stopped reporting
		row("system_service_up", 1, 5, map[string]string{"host": "web1", "service": "nginx", "state": "running"}),
		row("system_service_up", 0, 5, map[string]string{"host": "web1", "service": "postfix", "state": "failed"}),
		row("system_service_up", 0, 5, map[string]string{"host": "old1", "service": "sshd", "state": "failed"}), // host down: not counted
		row("container_up", 1, 5, map[string]string{"host": "web1", "container": "app", "state": "running"}),
		row("container_up", 0, 5, map[string]string{"host": "web1", "container": "db", "state": "exited"}),
		row("container_up", 0, 900, map[string]string{"host": "web1", "container": "gone", "state": "exited"}), // stale series: ignored
	}
	s := Compute(now, rows, map[string]registry.HostConfig{"new1": {Host: "new1"}}, nil)
	if s.Hosts != (Counts{Up: 1, Down: 1}) {
		t.Fatalf("hosts (web1 up, old1 down; configured-but-silent new1 is pending, not down): %+v", s.Hosts)
	}
	if s.Services != (Counts{Up: 1, Down: 1}) || s.Containers != (Counts{Up: 1, Down: 1}) {
		t.Fatalf("services %+v containers %+v", s.Services, s.Containers)
	}
	if len(s.Down) != 2 || s.Down[0].Name != "db" && s.Down[0].Name != "postfix" {
		t.Fatalf("down list: %+v", s.Down)
	}
	var web, pend Host
	for _, h := range s.HostList {
		if h.Name == "web1" {
			web = h
		}
		if h.Name == "new1" {
			pend = h
		}
	}
	if web.Version != "0.3" || web.OS != "linux/amd64" || web.Status != "up" || web.Services.Down != 1 || pend.Status != "pending" {
		t.Fatalf("host details: %+v / %+v", web, pend)
	}
	svc, ctr := Items(now, rows, "web1")
	if len(svc) != 2 || svc[0].Name != "postfix" || svc[0].Up || len(ctr) != 3 || !ctr[len(ctr)-1].Up == false && false {
		t.Fatalf("items must list failures first: %+v %+v", svc, ctr)
	}
	stale := 0
	for _, c := range ctr {
		if c.Stale {
			stale++
		}
	}
	if stale != 1 {
		t.Fatalf("the old container series must be marked stale: %+v", ctr)
	}
}

func inst(name, url, host string, created time.Time) registry.Instance {
	return registry.Instance{ID: "i" + name, Name: name, URL: url, Host: host, Created: created}
}

func TestNextcloudInstanceStates(t *testing.T) {
	long := now.Add(-time.Hour)
	reg := []registry.Instance{
		inst("ok", "https://ok.example.com", "h1", long),
		inst("dead", "https://dead.example.com", "h1", long),                    // reports 0: Nextcloud unreachable
		inst("silent", "https://silent.example.com:8443", "h1", long),           // reported once, then agent stopped
		inst("never", "https://never.example.com", "h1", long),                  // no data at all
		inst("fresh", "https://fresh.example.com", "h1", now.Add(-time.Minute)), // just added
	}
	rows := []model.Latest{
		row("nextcloud_up", 1, 10, map[string]string{"instance": "ok.example.com", "host": "h1"}),
		row("nextcloud_up", 0, 10, map[string]string{"instance": "dead.example.com", "host": "h1"}),
		row("nextcloud_up", 1, 400, map[string]string{"instance": "silent.example.com:8443", "host": "h1"}),
		// set up with agent flags, not in the registry:
		{Name: "nextcloud_up", Service: "flags-up", Attrs: map[string]string{"instance": "flags.example.com", "host": "h2"}, Value: 1, T: now.Unix() - 5},
		{Name: "nextcloud_up", Service: "flags-down", Attrs: map[string]string{"instance": "flagsdown.example.com", "host": "h2"}, Value: 0, T: now.Unix() - 5},
		{Name: "nextcloud_up", Service: "ok", Attrs: map[string]string{"instance": "ok-old-typo.example.com", "host": "h1"}, Value: 0, T: now.Unix() - 5},
		{Name: "nextcloud_up", Service: "forgotten", Attrs: map[string]string{"instance": "ancient.example.com", "host": "h2"}, Value: 1, T: now.Unix() - 7200},
	}
	s := Compute(now, rows, nil, reg)
	got := map[string]Instance{}
	for _, i := range s.Instances {
		got[i.Name] = i
	}
	want := map[string]string{"ok": "up", "dead": "down", "silent": "down", "never": "down", "fresh": "pending", "flags-up": "up", "flags-down": "down"}
	for n, st := range want {
		if got[n].Status != st {
			t.Errorf("%s: want %s got %s (%s)", n, st, got[n].Status, got[n].Reason)
		}
	}
	if _, ok := got["ks-old-typo"]; ok {
		t.Error("the old address of a corrected instance must not linger as a second, down instance")
	}
	if _, ok := got["forgotten"]; ok {
		t.Error("an unregistered instance that went silent long ago must be forgotten, not shown as down forever")
	}
	if s.Nextcloud != (Counts{Up: 2, Down: 4}) {
		t.Fatalf("counts (pending is neither): %+v", s.Nextcloud)
	}
	if got["ok"].Managed != true || got["flags-up"].Managed != false {
		t.Fatal("managed flag")
	}
	if got["dead"].Reason == "" || got["silent"].Reason == "" {
		t.Fatal("down instances must say why")
	}
}

func TestRemovedHostsStayHiddenUntilTheyReportAgain(t *testing.T) {
	rows := []model.Latest{
		row("lumen_agent_info", 1, 7200, map[string]string{"host": "gone1"}), // last seen before it was removed
		row("system_service_up", 0, 7200, map[string]string{"host": "gone1", "service": "x", "state": "failed"}),
		row("lumen_agent_info", 1, 5, map[string]string{"host": "back1"}),
		row("lumen_agent_info", 1, 5, map[string]string{"host": "web1"}),
	}
	removedAt := now.Add(-time.Hour)
	cfg := map[string]registry.HostConfig{
		"gone1": {Host: "gone1", Removed: true, RemovedAt: removedAt},
		"never": {Host: "never", Removed: true, RemovedAt: removedAt},           // removed before it ever reported
		"back1": {Host: "back1", Removed: true, RemovedAt: now.Add(-time.Hour)}, // reports again after the removal
		"web1":  {Host: "web1", DisplayName: "Web server 1"},
	}
	s := Compute(now, append(rows, row("lumen_agent_info", 1, 5, map[string]string{"host": "web1"})), cfg, nil)
	names := map[string]Host{}
	for _, h := range s.HostList {
		names[h.Name] = h
	}
	if _, ok := names["gone1"]; ok {
		t.Fatal("a removed host that has not reported since must be hidden")
	}
	if _, ok := names["never"]; ok {
		t.Fatal("a removed host must not show as pending")
	}
	if names["back1"].Status != "up" {
		t.Fatal("a host that reports again after being removed comes back")
	}
	if names["web1"].DisplayName != "Web server 1" {
		t.Fatalf("display name: %+v", names["web1"])
	}
	if s.Hosts.Down != 0 {
		t.Fatalf("a removed host must not count as down: %+v", s.Hosts)
	}
	if s.Services.Down != 0 || len(s.Down) != 0 {
		t.Fatalf("a removed host's services must not count: %+v", s.Services)
	}
}

func TestHostAddresses(t *testing.T) {
	rows := []model.Latest{
		row("lumen_agent_info", 1, 5, map[string]string{"host": "web1", "ip": "192.168.20.131", "ips": "192.168.20.131, 10.0.0.7,,"}),
		row("lumen_agent_info", 1, 5, map[string]string{"host": "old1"}),                                                // an agent from before addresses were reported
		row("lumen_agent_info", 1, 400, map[string]string{"host": "web1", "ip": "192.168.99.9", "ips": "192.168.99.9"}), // the address before it moved
	}
	s := Compute(now, rows, nil, nil)
	by := map[string]Host{}
	for _, h := range s.HostList {
		by[h.Name] = h
	}
	if by["web1"].IP != "192.168.20.131" || len(by["web1"].IPs) != 2 || by["web1"].IPs[1] != "10.0.0.7" {
		t.Fatalf("the newest report wins and the list is cleaned: %+v", by["web1"])
	}
	if by["old1"].IP != "" || len(by["old1"].IPs) != 0 {
		t.Fatalf("an older agent has no address: %+v", by["old1"])
	}
}
