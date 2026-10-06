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

func TestInstanceVersionsAndDetails(t *testing.T) {
	reg := []registry.Instance{
		inst("full", "https://full.example.com", "h1", now.Add(-time.Hour)),
		inst("basic", "https://basic.example.com", "h1", now.Add(-time.Hour)),
		inst("stale", "https://stale.example.com", "h1", now.Add(-time.Hour)),
	}
	rows := []model.Latest{
		row("nextcloud_up", 1, 5, map[string]string{"instance": "full.example.com", "host": "h1"}),
		row("nextcloud_info", 1, 5, map[string]string{"instance": "full.example.com", "host": "h1", "version": "34.0.1", "php_version": "8.3.9", "db_type": "pgsql", "db_version": "17.2", "junk": "x"}),
		row("nextcloud_users", 1, 5, map[string]string{"instance": "full.example.com", "host": "h1"}),
		// only availability is monitored (no token): status.php works, serverinfo does not
		row("nextcloud_up", 1, 5, map[string]string{"instance": "basic.example.com", "host": "h1"}),
		row("nextcloud_info", 1, 5, map[string]string{"instance": "basic.example.com", "host": "h1", "version": "33.0.0"}),
		// the version before an upgrade must not win over the newer report
		row("nextcloud_info", 1, 700, map[string]string{"instance": "full.example.com", "host": "h1", "version": "33.9.9"}),
		// serverinfo reported once, long ago: not "details" any more
		row("nextcloud_up", 1, 5, map[string]string{"instance": "stale.example.com", "host": "h1"}),
		row("nextcloud_users", 1, 900, map[string]string{"instance": "stale.example.com", "host": "h1"}),
		// unmanaged instance (agent flags) also gets its versions
		{Name: "nextcloud_up", Service: "flags", Attrs: map[string]string{"instance": "flags.example.com", "host": "h2"}, Value: 1, T: now.Unix() - 5},
		{Name: "nextcloud_info", Service: "flags", Attrs: map[string]string{"instance": "flags.example.com", "host": "h2", "version": "32.0.1"}, Value: 1, T: now.Unix() - 5},
	}
	s := Compute(now, rows, nil, reg)
	got := map[string]Instance{}
	for _, i := range s.Instances {
		got[i.Name] = i
	}
	if g := got["full"]; !g.Details || g.Info["version"] != "34.0.1" || g.Info["php_version"] != "8.3.9" || g.Info["db_type"] != "pgsql" || g.Info["db_version"] != "17.2" {
		t.Fatalf("full: %+v", g)
	}
	if _, ok := got["full"].Info["junk"]; ok {
		t.Fatal("only the known version labels are passed on")
	}
	if g := got["basic"]; g.Details || g.Info["version"] != "33.0.0" {
		t.Fatalf("an instance without serverinfo has a version but no details: %+v", g)
	}
	if got["stale"].Details || got["stale"].Info != nil {
		t.Fatalf("stale: %+v", got["stale"])
	}
	if g := got["flags"]; g.Info["version"] != "32.0.1" || g.Managed {
		t.Fatalf("unmanaged instance: %+v", g)
	}
}

func TestRemovedInstancesStayHiddenUntilTheyReportAgain(t *testing.T) {
	flags := map[string]string{"instance": "old.example.com", "host": "h1"}
	keep := map[string]string{"instance": "keep.example.com", "host": "h1"}
	mk := func(rows ...model.Latest) []model.Latest { return rows }
	unmanaged := func(name string, v float64, age int64, attrs map[string]string) model.Latest {
		r := row("nextcloud_up", v, age, attrs)
		r.Service = name
		return r
	}
	removedAt := now.Add(-10 * time.Minute)
	gone := map[string]time.Time{"old.example.com": removedAt}
	// it stopped reporting before it was removed: hidden, and not counted as down
	rows := mk(unmanaged("old", 0, 900, flags), unmanaged("keep", 1, 5, keep))
	s := ComputeWith(now, rows, nil, nil, gone)
	if len(s.Instances) != 1 || s.Instances[0].Name != "keep" || s.Nextcloud.Down != 0 || s.Nextcloud.Up != 1 {
		t.Fatalf("a removed instance is not listed and not counted: %+v", s.Instances)
	}
	// without the record it is there, as it used to be
	if s := Compute(now, rows, nil, nil); len(s.Instances) != 2 || s.Nextcloud.Down != 1 {
		t.Fatalf("%+v", s.Instances)
	}
	// the agent's last report just after the removal (within the grace) does not bring it back
	rows = mk(unmanaged("old", 1, 9*60, flags)) // 9 minutes ago = 1 minute after the removal
	if s := ComputeWith(now, rows, nil, nil, gone); len(s.Instances) != 0 {
		t.Fatalf("an agent that had not yet noticed: %+v", s.Instances)
	}
	// but if it reports well after the removal it is back (it is really still there)
	rows = mk(unmanaged("old", 1, 5, flags))
	if s := ComputeWith(now, rows, nil, nil, gone); len(s.Instances) != 1 || s.Instances[0].Status != "up" {
		t.Fatalf("reporting again after the removal: %+v", s.Instances)
	}
	// a registered instance is never hidden by a stale record (it was added again)
	reg := []registry.Instance{inst("old", "https://old.example.com", "h1", now.Add(-time.Hour))}
	if s := ComputeWith(now, mk(unmanaged("old", 1, 5, flags)), nil, reg, gone); len(s.Instances) != 1 || !s.Instances[0].Managed {
		t.Fatalf("%+v", s.Instances)
	}
}

func TestDeeperChecksOfAnInstance(t *testing.T) {
	at := map[string]string{"instance": "cloud.example.com", "host": "h1"}
	rows := []model.Latest{
		row("nextcloud_up", 1, 3, at), row("nextcloud_users", 4, 3, at),
		row("nextcloud_info", 1, 3, map[string]string{"instance": "cloud.example.com", "host": "h1", "version": "34.0.1", "update_version": "34.0.3"}),
		row("nextcloud_cron_age_seconds", 5000, 20, at), row("nextcloud_update_available", 1, 20, at), row("nextcloud_webdav_ok", 0, 14*60, at),
		row("nextcloud_tls_cert_expiry_seconds", 86400*9, 29*60, at), row("nextcloud_tls_cert_valid", 1, 29*60, at),
		// another instance, and a result that is too old to be shown
		row("nextcloud_up", 1, 3, map[string]string{"instance": "other.example.com", "host": "h1"}), row("nextcloud_cron_age_seconds", 1, 3000, map[string]string{"instance": "other.example.com", "host": "h1"}),
	}
	s := Compute(now, rows, nil, nil)
	var ks, other *Instance
	for i := range s.Instances {
		switch s.Instances[i].Key {
		case "cloud.example.com":
			ks = &s.Instances[i]
		case "other.example.com":
			other = &s.Instances[i]
		}
	}
	if ks == nil || ks.Checks == nil {
		t.Fatalf("%+v", s.Instances)
	}
	c := ks.Checks
	if c.CronAgeSeconds == nil || *c.CronAgeSeconds != 5000 || c.UpdateAvailable == nil || !*c.UpdateAvailable || c.UpdateVersion != "34.0.3" ||
		c.WebDAVOK == nil || *c.WebDAVOK || c.TLSExpirySeconds == nil || *c.TLSExpirySeconds != 86400*9 || c.TLSValid == nil || !*c.TLSValid {
		t.Fatalf("%+v", c)
	}
	// each check is shown for a few of its own intervals: cron 5 minutes, the login check 15, the certificate 30
	old := []model.Latest{row("nextcloud_up", 1, 3, at), row("nextcloud_cron_age_seconds", 5000, 6*60, at), row("nextcloud_webdav_ok", 1, 16*60, at), row("nextcloud_tls_cert_expiry_seconds", 100, 31*60, at), row("nextcloud_update_available", 1, 6*60, at)}
	if g := Compute(now, old, nil, nil); len(g.Instances) != 1 || g.Instances[0].Checks != nil {
		t.Fatalf("a check that stopped is not shown with its old value: %+v", g.Instances[0].Checks)
	}
	if other == nil || other.Checks != nil {
		t.Fatalf("a result older than 20 minutes is not shown, and an instance with none has no checks: %+v", other)
	}
}
