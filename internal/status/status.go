// Package status turns the latest sample of each series into up/down answers for hosts, services, containers and
// Nextcloud instances. "Down" means the thing reports a failure, or it used to report and stopped.
package status

import (
	"sort"
	"strings"
	"time"

	"github.com/danielingemar/lumen/internal/model"
	"github.com/danielingemar/lumen/internal/registry"
)

// Metric names the status view needs.
var Names = []string{"lumen_agent_info", "lumen_agent_log_paths_rejected", "nextcloud_up", "nextcloud_info", "nextcloud_users", "system_service_up", "container_up",
	"nextcloud_cron_age_seconds", "nextcloud_update_available", "nextcloud_webdav_ok", "nextcloud_tls_cert_expiry_seconds", "nextcloud_tls_cert_valid"}

const (
	Fresh      = 120 * time.Second // a sample newer than this counts as "reporting now"
	GoneAfter  = 15 * time.Minute  // an instance nobody registered that went silent this long ago is forgotten
	PendingFor = 5 * time.Minute   // a newly registered instance gets this long to deliver its first sample
)

type Counts struct {
	Up   int `json:"up"`
	Down int `json:"down"`
}

type Item struct {
	Kind     string `json:"kind"` // service | container
	Name     string `json:"name"`
	Host     string `json:"host,omitempty"`
	State    string `json:"state,omitempty"`
	Up       bool   `json:"up"`
	LastSeen int64  `json:"last_seen"`
	Stale    bool   `json:"stale,omitempty"` // not reported recently: the state is the last one known
}

type Host struct {
	Name             string   `json:"name"`
	DisplayName      string   `json:"display_name"`
	Status           string   `json:"status"` // up | down | pending
	Version          string   `json:"version"`
	SelfUpdate       string   `json:"self_update,omitempty"` // how the agent can update itself ("systemd", "exec"), empty if it cannot
	OS               string   `json:"os"`
	IP               string   `json:"ip"`  // the address the machine uses to reach Lumen
	IPs              []string `json:"ips"` // its other usable addresses (the agent reports them)
	LastSeen         int64    `json:"last_seen"`
	Services         Counts   `json:"services"`
	Containers       Counts   `json:"containers"`
	RejectedLogPaths int      `json:"rejected_log_paths,omitempty"` // log paths this machine refused to read (agent allow-list)
	Role             string   `json:"role,omitempty"`               // "checker": an agent that only checks Nextcloud instances; it is not a machine
}

type Instance struct {
	registry.InstanceOut
	Status   string            `json:"status"` // up | down | pending
	Reason   string            `json:"reason,omitempty"`
	LastSeen int64             `json:"last_seen"`
	Managed  bool              `json:"managed"` // false: reports to us but was set up with agent flags, not in the UI
	Label    string            `json:"label,omitempty"`
	Info     map[string]string `json:"info,omitempty"` // Nextcloud, PHP and database versions, as reported by the agent
	Details  bool              `json:"details"`        // users, files, storage... are reported (a serverinfo token or login works)
	Checks   *Checks           `json:"checks,omitempty"`
}

// Checks are the results of the deeper checks of an instance, as far as they are reported. A nil field means "not
// reported" (for example background jobs need an administrator login), which is not the same as a bad result.
type Checks struct {
	CronAgeSeconds   *float64 `json:"cron_age_seconds,omitempty"`   // since Nextcloud's cron last ran
	UpdateAvailable  *bool    `json:"update_available,omitempty"`   // a newer Nextcloud version exists
	UpdateVersion    string   `json:"update_version,omitempty"`     // which
	WebDAVOK         *bool    `json:"webdav_ok,omitempty"`          // a user can log in and list their files
	TLSExpirySeconds *float64 `json:"tls_expiry_seconds,omitempty"` // until the certificate expires (negative: it has)
	TLSValid         *bool    `json:"tls_valid,omitempty"`          // trusted and for this name
}

// checkFresh is how old a deeper check may be and still be shown: a few of its own intervals (cron and updates are
// collected every minute or so, the login check every five minutes, the certificate every ten), so that a check that
// stops, for example because the administrator login was removed, disappears instead of showing an old value.
func checkFresh(name string) time.Duration {
	switch name {
	case "nextcloud_webdav_ok":
		return 15 * time.Minute
	case "nextcloud_tls_cert_expiry_seconds", "nextcloud_tls_cert_valid":
		return 30 * time.Minute
	}
	return 5 * time.Minute
}

type Summary struct {
	Hosts      Counts     `json:"hosts"`
	Nextcloud  Counts     `json:"nextcloud"`
	Services   Counts     `json:"services"`
	Containers Counts     `json:"containers"`
	HostList   []Host     `json:"host_list"`
	Checkers   []Host     `json:"checkers"` // agents that only check instances: they are not machines, and are listed with the instances
	Instances  []Instance `json:"instances"`
	Down       []Item     `json:"down"` // services and containers that are down, for the "what is broken" list
}

func (c *Counts) add(up bool) {
	if up {
		c.Up++
	} else {
		c.Down++
	}
}

// Compute builds the summary. configured are the hosts that have a stored configuration (so they show up even
// before they report); instances are the registered Nextcloud instances.
func Compute(now time.Time, rows []model.Latest, configured map[string]registry.HostConfig, instances []registry.Instance) Summary {
	return ComputeWith(now, rows, configured, instances, nil)
}

// RemovedGrace is how long after an instance was removed its agent may still report before that counts as the instance
// coming back (an agent learns about a removal within a minute).
const RemovedGrace = 2 * time.Minute

// ComputeWith is Compute that also knows which instances were removed and when: those are not listed until they report
// again after the removal.
func ComputeWith(now time.Time, rows []model.Latest, configured map[string]registry.HostConfig, instances []registry.Instance, gone map[string]time.Time) Summary {
	age := func(t int64) time.Duration { return now.Sub(time.Unix(t, 0)) }
	hosts := map[string]*Host{}
	host := func(n string) *Host {
		if hosts[n] == nil {
			hosts[n] = &Host{Name: n, Status: "down"}
		}
		return hosts[n]
	}
	for _, r := range rows {
		if r.Name == "lumen_agent_info" && r.Attrs["host"] != "" {
			h := host(r.Attrs["host"])
			if r.T >= h.LastSeen {
				h.LastSeen, h.Version, h.OS, h.SelfUpdate, h.Role = r.T, r.Attrs["version"], r.Attrs["os"], r.Attrs["self_update"], r.Attrs["role"]
				h.IP, h.IPs = r.Attrs["ip"], nil
				for _, a := range strings.Split(r.Attrs["ips"], ",") {
					if a = strings.TrimSpace(a); a != "" && len(a) <= 45 {
						h.IPs = append(h.IPs, a)
					}
				}
			}
		}
	}
	for _, r := range rows {
		if r.Name == "lumen_agent_log_paths_rejected" && r.Attrs["host"] != "" && now.Sub(time.Unix(r.T, 0)) <= Fresh {
			if h := hosts[r.Attrs["host"]]; h != nil {
				h.RejectedLogPaths = int(r.Value)
			}
		}
	}
	for n, c := range configured {
		if c.Removed {
			// stays hidden until the machine reports again after it was removed
			if h := hosts[n]; h != nil && h.LastSeen > c.RemovedAt.Unix() {
				h.DisplayName = c.DisplayName
			} else {
				delete(hosts, n)
			}
			continue
		}
		if hosts[n] == nil {
			host(n).Status = "pending"
		}
		hosts[n].DisplayName = c.DisplayName
	}
	for _, h := range hosts {
		if h.LastSeen > 0 && age(h.LastSeen) <= Fresh {
			h.Status = "up"
		}
	}
	var s Summary
	// services and containers count only on hosts that are reporting: when a host is down, the host is the problem
	for _, r := range rows {
		hn := r.Attrs["host"]
		h := hosts[hn]
		if h == nil || h.Status != "up" || age(r.T) > Fresh {
			continue
		}
		up := r.Value >= 1
		switch r.Name {
		case "system_service_up":
			s.Services.add(up)
			h.Services.add(up)
			if !up {
				s.Down = append(s.Down, Item{Kind: "service", Name: r.Attrs["service"], Host: hn, State: r.Attrs["state"], LastSeen: r.T})
			}
		case "container_up":
			s.Containers.add(up)
			h.Containers.add(up)
			if !up {
				s.Down = append(s.Down, Item{Kind: "container", Name: r.Attrs["container"], Host: hn, State: r.Attrs["state"], LastSeen: r.T})
			}
		}
	}
	for _, h := range hosts {
		if h.Role == "checker" { // not a machine: not listed, not counted
			s.Checkers = append(s.Checkers, *h)
			continue
		}
		switch h.Status {
		case "up":
			s.Hosts.Up++
		case "down":
			s.Hosts.Down++
		}
		s.HostList = append(s.HostList, *h)
	}
	sort.Slice(s.HostList, func(i, j int) bool { return s.HostList[i].Name < s.HostList[j].Name })
	sort.Slice(s.Checkers, func(i, j int) bool { return s.Checkers[i].Name < s.Checkers[j].Name })
	sort.Slice(s.Down, func(i, j int) bool { return s.Down[i].Host+s.Down[i].Name < s.Down[j].Host+s.Down[j].Name })

	// Nextcloud: match what is registered against what reports
	byKey := map[string]model.Latest{}
	for _, r := range rows {
		if r.Name == "nextcloud_up" {
			if k := r.Attrs["instance"]; k != "" && r.T >= byKey[k].T {
				byKey[k] = r
			}
		}
	}
	// versions (nextcloud_info labels) and whether serverinfo works (nextcloud_users is reported) per instance
	infoRow, detailRow := map[string]model.Latest{}, map[string]model.Latest{}
	checkRow := map[string]map[string]model.Latest{} // metric name -> instance -> newest row
	for _, r := range rows {
		k := r.Attrs["instance"]
		if strings.HasPrefix(r.Name, "nextcloud_cron_") || strings.HasPrefix(r.Name, "nextcloud_tls_") || r.Name == "nextcloud_update_available" || r.Name == "nextcloud_webdav_ok" {
			if k != "" && age(r.T) <= checkFresh(r.Name) {
				if checkRow[r.Name] == nil {
					checkRow[r.Name] = map[string]model.Latest{}
				}
				if r.T >= checkRow[r.Name][k].T {
					checkRow[r.Name][k] = r
				}
			}
		}
		switch r.Name {
		case "nextcloud_info":
			if k != "" && r.T >= infoRow[k].T {
				infoRow[k] = r
			}
		case "nextcloud_users":
			if k != "" && r.T >= detailRow[k].T {
				detailRow[k] = r
			}
		}
	}
	decorate := func(in *Instance, key string) {
		if r, ok := infoRow[key]; ok {
			in.Info = map[string]string{}
			for _, f := range []string{"version", "php_version", "db_type", "db_version"} {
				if v := r.Attrs[f]; v != "" && len(v) <= 60 {
					in.Info[f] = v
				}
			}
		}
		if r, ok := detailRow[key]; ok && age(r.T) <= Fresh {
			in.Details = true
		}
		var c Checks
		have := false
		f := func(name string) (float64, bool) {
			r, ok := checkRow[name][key]
			return r.Value, ok
		}
		if v, ok := f("nextcloud_cron_age_seconds"); ok {
			c.CronAgeSeconds, have = &v, true
		}
		if v, ok := f("nextcloud_update_available"); ok {
			b := v >= 1
			c.UpdateAvailable, have = &b, true
			if r, ok := infoRow[key]; ok && b {
				if uv := r.Attrs["update_version"]; uv != "" && len(uv) <= 40 {
					c.UpdateVersion = uv
				}
			}
		}
		if v, ok := f("nextcloud_webdav_ok"); ok {
			b := v >= 1
			c.WebDAVOK, have = &b, true
		}
		if v, ok := f("nextcloud_tls_cert_expiry_seconds"); ok {
			c.TLSExpirySeconds, have = &v, true
		}
		if v, ok := f("nextcloud_tls_cert_valid"); ok {
			b := v >= 1
			c.TLSValid, have = &b, true
		}
		if have {
			in.Checks = &c
		}
	}
	seen := map[string]bool{}
	add := func(in Instance) {
		s.Instances = append(s.Instances, in)
		switch in.Status {
		case "up":
			s.Nextcloud.Up++
		case "down":
			s.Nextcloud.Down++
		}
	}
	for _, i := range instances {
		o := i.Out()
		in := Instance{InstanceOut: o, Managed: true, Status: "down", Reason: "not reporting: is the agent on " + i.Host + " running?"}
		decorate(&in, o.Key)
		if r, ok := byKey[o.Key]; ok {
			seen[o.Key] = true
			in.LastSeen = r.T
			switch {
			case age(r.T) > Fresh:
				in.Reason = "stopped reporting " + age(r.T).Round(time.Second).String() + " ago: the agent on " + i.Host + " may be down"
			case r.Value >= 1:
				in.Status, in.Reason = "up", ""
			default:
				in.Reason = "the agent cannot reach this Nextcloud (check the URL, DNS, firewall)"
			}
		} else if now.Sub(i.Created) < PendingFor {
			in.Status, in.Reason = "pending", "waiting for the first check from "+i.Host
		}
		add(in)
	}
	names := map[string]bool{}
	for _, i := range instances {
		names[i.Name] = true
	}
	for k, r := range byKey { // instances that report but were set up with agent flags
		// the old address of an instance whose URL was just corrected in the UI still reports under the same name
		if seen[k] || age(r.T) > GoneAfter || names[r.Service] {
			continue
		}
		if at, removed := gone[k]; removed && r.T <= at.Add(RemovedGrace).Unix() {
			continue // removed, and it has not reported since
		}
		in := Instance{InstanceOut: registry.InstanceOut{Name: r.Service, URL: "", Host: r.Attrs["host"], Key: k}, Managed: false, LastSeen: r.T, Status: "down"}
		decorate(&in, k)
		switch {
		case age(r.T) > Fresh:
			in.Reason = "stopped reporting"
		case r.Value >= 1:
			in.Status = "up"
		default:
			in.Reason = "the agent cannot reach this Nextcloud"
		}
		add(in)
	}
	sort.Slice(s.Instances, func(i, j int) bool { return s.Instances[i].Name < s.Instances[j].Name })
	return s
}

// Items lists the services and containers last reported by one host, failures first.
func Items(now time.Time, rows []model.Latest, host string) (services, containers []Item) {
	for _, r := range rows {
		if r.Attrs["host"] != host {
			continue
		}
		stale := now.Sub(time.Unix(r.T, 0)) > Fresh
		switch r.Name {
		case "system_service_up":
			services = append(services, Item{Kind: "service", Name: r.Attrs["service"], Host: host, State: r.Attrs["state"], Up: r.Value >= 1, LastSeen: r.T, Stale: stale})
		case "container_up":
			containers = append(containers, Item{Kind: "container", Name: r.Attrs["container"], Host: host, State: r.Attrs["state"], Up: r.Value >= 1, LastSeen: r.T, Stale: stale})
		}
	}
	order := func(x []Item) {
		sort.Slice(x, func(i, j int) bool {
			if x[i].Up != x[j].Up {
				return !x[i].Up
			}
			return x[i].Name < x[j].Name
		})
	}
	order(services)
	order(containers)
	return
}
