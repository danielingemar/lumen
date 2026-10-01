// Package status turns the latest sample of each series into up/down answers for hosts, services, containers and
// Nextcloud instances. "Down" means the thing reports a failure, or it used to report and stopped.
package status

import (
	"sort"
	"time"

	"github.com/danielingemar/lumen/internal/model"
	"github.com/danielingemar/lumen/internal/registry"
)

// Metric names the status view needs.
var Names = []string{"lumen_agent_info", "lumen_agent_log_paths_rejected", "nextcloud_up", "system_service_up", "container_up"}

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
	Name             string `json:"name"`
	Status           string `json:"status"` // up | down | pending
	Version          string `json:"version"`
	OS               string `json:"os"`
	LastSeen         int64  `json:"last_seen"`
	Services         Counts `json:"services"`
	Containers       Counts `json:"containers"`
	RejectedLogPaths int    `json:"rejected_log_paths,omitempty"` // log paths this machine refused to read (agent allow-list)
}

type Instance struct {
	registry.InstanceOut
	Status   string `json:"status"` // up | down | pending
	Reason   string `json:"reason,omitempty"`
	LastSeen int64  `json:"last_seen"`
	Managed  bool   `json:"managed"` // false: reports to us but was set up with agent flags, not in the UI
	Label    string `json:"label,omitempty"`
}

type Summary struct {
	Hosts      Counts     `json:"hosts"`
	Nextcloud  Counts     `json:"nextcloud"`
	Services   Counts     `json:"services"`
	Containers Counts     `json:"containers"`
	HostList   []Host     `json:"host_list"`
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
				h.LastSeen, h.Version, h.OS = r.T, r.Attrs["version"], r.Attrs["os"]
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
	for n := range configured {
		if hosts[n] == nil {
			host(n).Status = "pending"
		}
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
		switch h.Status {
		case "up":
			s.Hosts.Up++
		case "down":
			s.Hosts.Down++
		}
		s.HostList = append(s.HostList, *h)
	}
	sort.Slice(s.HostList, func(i, j int) bool { return s.HostList[i].Name < s.HostList[j].Name })
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
		in := Instance{InstanceOut: registry.InstanceOut{Name: r.Service, URL: "", Host: r.Attrs["host"], Key: k}, Managed: false, LastSeen: r.T, Status: "down"}
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
