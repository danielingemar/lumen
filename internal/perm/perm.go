// Package perm defines what a user or key may do: a level (none, read, write) per area of the product.
package perm

import (
	"fmt"
	"sort"
)

const (
	Dashboards    = "dashboards"
	Traces        = "traces"
	Logs          = "logs"
	Metrics       = "metrics"
	Hosts         = "hosts" // hosts, instances and their agent configuration
	Keys          = "keys"  // agent API keys
	Users         = "users" // users and groups
	Backups       = "backups"
	Billing       = "billing"       // prices, and the basis for invoices (it shows what each customer pays)
	Settings      = "settings"      // site name and logo
	Alerts        = "alerts"        // alert rules, acknowledging and silences
	Notifications = "notifications" // channels that send messages (they hold secrets and reach the network)
	Operator      = "operator"      // the tenant console: only ever granted inside the operator tenant
)

const (
	None  = "none"
	Read  = "read"
	Write = "write"
)

type Area struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Help     string `json:"help"`
	Writable bool   `json:"writable"` // telemetry areas are read-only by nature
}

// Areas is the catalogue shown in the group editor, in display order.
var Areas = []Area{
	{Dashboards, "Dashboards", "View dashboards. Write: create, edit and delete them.", true},
	{Traces, "Traces", "Search and open traces.", false},
	{Logs, "Logs", "Search and read logs.", false},
	{Metrics, "Metrics", "Metrics explorer and chart data.", false},
	{Hosts, "Hosts & instances", "See hosts and Nextcloud instances. Write: change their settings (log paths, URLs, tokens).", true},
	{Keys, "Agent keys", "See agent keys. Write: create and delete them.", true},
	{Users, "Users & groups", "See users and groups. Write: create, change and delete them.", true},
	{Backups, "Backups & archive", "View archived data. Write: load or unload archived days and run a backup.", true},
	{Alerts, "Alerts", "See alerts, rules and silences. Write: manage rules and silences, acknowledge alerts.", true},
	{Notifications, "Notification channels", "See channels. Write: create channels (email, webhook, Slack...); they hold secrets and send messages from the server.", true},
	{Operator, "Operator console", "See tenants, their usage and limits. Write: create, suspend and remove tenants, set limits, and enter a tenant for support. Only in the operator tenant.", true},
	{Billing, "Billing", "See the usage and the invoice basis of the Nextcloud instances. Write: set prices, currency and VAT, and who is the customer of each instance.", true},
	{Settings, "Settings", "See the settings page. Write: change the site name and logo (shown to everyone, also on the login page).", true},
}

func filled(level func(Area) string) map[string]string {
	m := map[string]string{}
	for _, a := range Areas {
		m[a.ID] = level(a)
	}
	return m
}

// Admin may do everything.
func Admin() map[string]string {
	return filled(func(a Area) string {
		if a.Writable {
			return Write
		}
		return Read
	})
}

// User may read data, dashboards, hosts and instances, nothing else.
func User() map[string]string {
	return filled(func(a Area) string {
		switch a.ID {
		case Dashboards, Traces, Logs, Metrics, Hosts, Alerts:
			return Read
		}
		return None
	})
}

// Key is what an API key may do: read all data, and manage dashboards (for scripting). Agents also use it
// to fetch their own configuration.
func Key() map[string]string {
	return filled(func(a Area) string {
		switch a.ID {
		case Dashboards:
			return Write
		case Traces, Logs, Metrics, Hosts:
			return Read
		}
		return None
	})
}

// Normalize2None is a permission map with every area set to none.
func Normalize2None() map[string]string { return filled(func(Area) string { return None }) }

// Normalize validates a permission map from a request and fills in missing areas with "none".
// Write on a read-only area (telemetry) is stored as read.
func Normalize(in map[string]string) (map[string]string, error) {
	known := map[string]Area{}
	for _, a := range Areas {
		known[a.ID] = a
	}
	out := filled(func(Area) string { return None })
	for k, v := range in {
		a, ok := known[k]
		if !ok {
			return nil, fmt.Errorf("unknown area %q", k)
		}
		switch v {
		case None, Read:
		case Write:
			if !a.Writable {
				v = Read
			}
		default:
			return nil, fmt.Errorf("level for %s must be none, read or write", k)
		}
		out[k] = v
	}
	return out, nil
}

// Summary is a short human-readable description, e.g. "write: dashboards, hosts; read: traces, logs".
func Summary(p map[string]string) string {
	var w, r []string
	for _, a := range Areas {
		switch p[a.ID] {
		case Write:
			w = append(w, a.ID)
		case Read:
			r = append(r, a.ID)
		}
	}
	sort.Strings(w)
	s := ""
	if len(w) > 0 {
		s = "write: " + join(w)
	}
	if len(r) > 0 {
		if s != "" {
			s += "; "
		}
		s += "read: " + join(r)
	}
	if s == "" {
		return "no access"
	}
	return s
}

func join(x []string) string {
	s := ""
	for i, v := range x {
		if i > 0 {
			s += ", "
		}
		s += v
	}
	return s
}

// OperatorTenant is the reserved tenant of the people who run the installation. The operator area exists only here.
const OperatorTenant = "operator"

// ForTenant removes what a tenant may not have: the operator area outside the operator tenant.
func ForTenant(m map[string]string, tenant string) map[string]string {
	if tenant == OperatorTenant || m[Operator] == None || m[Operator] == "" {
		return m
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	out[Operator] = None
	return out
}

// ActingPerms is what an operator may do inside a tenant: everything a tenant administrator may, or only read.
// The operator console itself is not available while inside a tenant (leave first).
func ActingPerms(write bool) map[string]string {
	m := Admin()
	m[Operator] = None
	if !write {
		for k, v := range m {
			if v == Write {
				m[k] = Read
			}
		}
	}
	return m
}
