package status

import (
	"sort"
	"strings"
	"time"

	"github.com/inmagma/lumen/internal/model"
	"github.com/inmagma/lumen/internal/registry"
)

// Redis is one Redis server as the UI shows it. State is up, down, auth (it wants a password that is missing or wrong) or
// pending (set up in the UI, nothing reported yet).
type Redis struct {
	Host        string   `json:"host"`
	Name        string   `json:"name"`
	Container   string   `json:"container,omitempty"`
	Addr        string   `json:"addr,omitempty"`
	State       string   `json:"state"`
	Reason      string   `json:"reason,omitempty"`
	Version     string   `json:"version,omitempty"`
	Role        string   `json:"role,omitempty"`
	LastSeen    int64    `json:"last_seen"`
	Stale       bool     `json:"stale,omitempty"`
	RecordID    string   `json:"record_id,omitempty"` // set when it is configured in the UI (password or address)
	HasPassword bool     `json:"has_password"`
	Username    string   `json:"username,omitempty"`
	MemUsed     *float64 `json:"memory_used,omitempty"`
	MemMax      *float64 `json:"memory_max,omitempty"`
	Clients     *float64 `json:"clients,omitempty"`
	Keys        *float64 `json:"keys,omitempty"`
	Evicted     *float64 `json:"evicted,omitempty"`
	Hits        *float64 `json:"hits,omitempty"`
	Misses      *float64 `json:"misses,omitempty"`
	Uptime      *float64 `json:"uptime,omitempty"`
}

// RedisList builds the Redis list from the latest samples and the Redis set up in the UI.
func RedisList(now time.Time, rows []model.Latest, recs []registry.Redis) []Redis {
	type key struct{ host, name string }
	m := map[key]*Redis{}
	newest := map[key]int64{}
	get := func(k key) *Redis {
		if m[k] == nil {
			m[k] = &Redis{Host: k.host, Name: k.name, State: "down"}
		}
		return m[k]
	}
	for _, r := range rows {
		if !strings.HasPrefix(r.Name, "redis_") || r.Attrs["redis"] == "" || r.Attrs["host"] == "" {
			continue
		}
		k := key{r.Attrs["host"], r.Attrs["redis"]}
		x := get(k)
		v := r.Value
		switch r.Name {
		case "redis_up":
			if r.T >= newest[k] {
				newest[k] = r.T
				x.LastSeen = r.T
				if v >= 1 {
					x.State = "up"
				} else if x.State != "auth" {
					x.State = "down"
				}
			}
		case "redis_auth_required":
			if v >= 1 && r.T >= newest[k] {
				x.State = "auth"
			}
		case "redis_info":
			if x.Version == "" || r.T >= newest[k] {
				x.Container, x.Addr, x.Reason, x.Version, x.Role = r.Attrs["container"], r.Attrs["addr"], r.Attrs["reason"], r.Attrs["version"], r.Attrs["role"]
			}
		case "redis_memory_used_bytes":
			x.MemUsed = &v
		case "redis_memory_max_bytes":
			x.MemMax = &v
		case "redis_clients":
			x.Clients = &v
		case "redis_keys":
			x.Keys = &v
		case "redis_evicted_keys_total":
			x.Evicted = &v
		case "redis_keyspace_hits_total":
			x.Hits = &v
		case "redis_keyspace_misses_total":
			x.Misses = &v
		case "redis_uptime_seconds":
			x.Uptime = &v
		}
	}
	for k, x := range m {
		if x.State == "auth" && x.Reason == "" {
			x.Reason = "needs a password"
		}
		// numbers of a Redis that is not answering are old: do not show them as if they were current
		if x.State != "up" {
			x.MemUsed, x.MemMax, x.Clients, x.Keys, x.Evicted, x.Hits, x.Misses, x.Uptime = nil, nil, nil, nil, nil, nil, nil, nil
		}
		if now.Sub(time.Unix(x.LastSeen, 0)) > Fresh {
			x.Stale = true
		}
		_ = k
	}
	for _, rec := range recs {
		var x *Redis
		for k, c := range m {
			if k.host == rec.Host && (k.name == rec.Name || (rec.Container != "" && c.Container == rec.Container)) {
				x = c
			}
		}
		if x == nil {
			x = &Redis{Host: rec.Host, Name: rec.Name, State: "pending", Container: rec.Container, Addr: rec.Addr}
			m[key{rec.Host, rec.Name}] = x
		}
		x.RecordID, x.HasPassword, x.Username = rec.ID, rec.PasswordEnc != "", rec.Username
	}
	out := make([]Redis, 0, len(m))
	for _, x := range m {
		out = append(out, *x)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].State == "up") != (out[j].State == "up") {
			return out[i].State != "up"
		}
		return out[i].Host+"/"+out[i].Name < out[j].Host+"/"+out[j].Name
	})
	return out
}
