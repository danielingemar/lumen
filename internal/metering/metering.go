// Package metering counts what each tenant sends and uses: accepted items and bytes per signal and day, the hosts that
// reported, and the peak number of objects (users, keys...). The numbers are for the operator's own invoicing and for
// the limits of a tenant; the definition ("accepted OTLP payload bytes") is public so that a customer can check it.
package metering

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
)

const coll = "usage"

// Signal is what was accepted of one kind of telemetry.
type Signal struct {
	Items int64 `json:"items"`
	Bytes int64 `json:"bytes"`
}

// Day is one tenant's usage for one UTC day.
type Day struct {
	ID      string         `json:"id"`
	Tenant  string         `json:"tenant"`
	Day     string         `json:"day"` // 2026-10-06
	Traces  Signal         `json:"traces"`
	Logs    Signal         `json:"logs"`
	Metrics Signal         `json:"metrics"`
	Hosts   []string       `json:"hosts"` // the hosts that sent metrics this day
	Peak    map[string]int `json:"peak"`  // the highest number seen of hosts, instances, users, groups, keys, dashboards
	Updated time.Time      `json:"updated"`
}

func (d Day) Items() int64 { return d.Traces.Items + d.Logs.Items + d.Metrics.Items }
func (d Day) Bytes() int64 { return d.Traces.Bytes + d.Logs.Bytes + d.Metrics.Bytes }

const maxHostsPerDay = 5000

// Meter keeps the counters in memory and writes them to the document store once a minute (and on demand).
type Meter struct {
	DB  docstore.Backend
	Now func() time.Time

	mu     sync.Mutex
	days   map[string]*Day
	dirty  map[string]bool
	seen   map[string]map[string]time.Time // tenant -> host -> last seen
	loaded map[string]bool                 // tenants whose hosts have been read back after a restart
	check  map[string]map[string]bool      // tenant -> names of agents that only check instances (they are not hosts)
}

func New(db docstore.Backend) *Meter {
	return &Meter{DB: db, Now: time.Now, days: map[string]*Day{}, dirty: map[string]bool{}, seen: map[string]map[string]time.Time{}, loaded: map[string]bool{}, check: map[string]map[string]bool{}}
}

func ctx5() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 8*time.Second)
}

func (m *Meter) today() string { return m.Now().UTC().Format("2006-01-02") }

func key(tenant, day string) string { return tenant + "|" + day }

// day returns the day record, reading it back from the store the first time (so counting continues after a restart).
func (m *Meter) day(tenant, day string) *Day {
	k := key(tenant, day)
	if d, ok := m.days[k]; ok {
		return d
	}
	d := &Day{ID: k, Tenant: tenant, Day: day, Peak: map[string]int{}}
	c, cancel := ctx5()
	defer cancel()
	if doc, err := m.DB.Get(c, coll, k); err == nil {
		var old Day
		if doc.Decode(&old) == nil {
			d = &old
			if d.Peak == nil {
				d.Peak = map[string]int{}
			}
		}
	}
	m.days[k] = d
	return d
}

// hosts reads back the hosts that reported today and yesterday, once per tenant, so that "known host" survives a restart.
func (m *Meter) hostsLocked(tenant string) map[string]time.Time {
	if !m.loaded[tenant] {
		m.loaded[tenant] = true
		set := m.seen[tenant]
		if set == nil {
			set = map[string]time.Time{}
			m.seen[tenant] = set
		}
		for _, off := range []int{0, -1} {
			d := m.day(tenant, m.Now().UTC().AddDate(0, 0, off).Format("2006-01-02"))
			for _, h := range d.Hosts {
				if t, ok := set[h]; !ok || d.Updated.After(t) {
					set[h] = d.Updated
				}
			}
		}
	}
	return m.seen[tenant]
}

// Record counts accepted telemetry. signal is "traces", "logs" or "metrics"; hosts are the hosts a metrics request came from.
func (m *Meter) Record(tenant, signal string, items, bytes int, hosts []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.Now().UTC()
	d := m.day(tenant, m.today())
	var s *Signal
	switch signal {
	case "traces":
		s = &d.Traces
	case "logs":
		s = &d.Logs
	case "metrics":
		s = &d.Metrics
	default:
		return
	}
	s.Items += int64(items)
	s.Bytes += int64(bytes)
	set := m.hostsLocked(tenant)
	have := map[string]bool{}
	for _, h := range d.Hosts {
		have[h] = true
	}
	for _, h := range hosts {
		if h == "" || len(h) > 200 {
			continue
		}
		set[h] = now
		if !have[h] && len(d.Hosts) < maxHostsPerDay {
			d.Hosts = append(d.Hosts, h)
			have[h] = true
		}
	}
	d.Updated = now
	m.dirty[d.ID] = true
}

// Today is a copy of the tenant's usage so far today.
func (m *Meter) Today(tenant string) Day {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := *m.day(tenant, m.today())
	d.Hosts = append([]string(nil), d.Hosts...)
	return d
}

// BytesToday is how many bytes the tenant has had accepted today.
func (m *Meter) BytesToday(tenant string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.day(tenant, m.today()).Bytes()
}

// ActiveHosts is the number of hosts that have sent metrics in the last 24 hours.
func (m *Meter) ActiveHosts(tenant string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, cut := 0, m.Now().Add(-24*time.Hour)
	for _, t := range m.hostsLocked(tenant) {
		if t.After(cut) {
			n++
		}
	}
	return n
}

// NewHosts is how many of these hosts have not sent anything in the last 24 hours.
func (m *Meter) NewHosts(tenant string, hosts []string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	set, cut, seen, n := m.hostsLocked(tenant), m.Now().Add(-24*time.Hour), map[string]bool{}, 0
	for _, h := range hosts {
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		if t, ok := set[h]; !ok || !t.After(cut) {
			n++
		}
	}
	return n
}

// Sample notes the number of objects a tenant has now, and keeps the highest of the day.
func (m *Meter) Sample(tenant string, objects map[string]int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.day(tenant, m.today())
	for k, v := range objects {
		if v > d.Peak[k] {
			d.Peak[k] = v
		}
	}
	d.Updated = m.Now().UTC()
	m.dirty[d.ID] = true
}

// Flush writes the days that changed.
func (m *Meter) Flush() {
	m.mu.Lock()
	var out []Day
	for k := range m.dirty {
		if d, ok := m.days[k]; ok {
			c := *d
			c.Hosts = append([]string(nil), d.Hosts...)
			out = append(out, c)
		}
	}
	m.dirty = map[string]bool{}
	m.mu.Unlock()
	for _, d := range out {
		c, cancel := ctx5()
		if err := m.DB.Put(c, coll, d.ID, d, ""); err != nil {
			m.mu.Lock()
			m.dirty[d.ID] = true // try again next time
			m.mu.Unlock()
		}
		cancel()
	}
}

// Range returns the days from..to (inclusive, YYYY-MM-DD) of one tenant, or of all when tenant is empty, oldest first.
func (m *Meter) Range(tenant, from, to string) []Day {
	m.Flush()
	c, cancel := ctx5()
	defer cancel()
	var f map[string]string
	if tenant != "" {
		f = map[string]string{"tenant": tenant}
	}
	docs, _ := m.DB.List(c, coll, f, 100000)
	var out []Day
	for _, doc := range docs {
		var d Day
		if doc.Decode(&d) != nil || (from != "" && d.Day < from) || (to != "" && d.Day > to) {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day < out[j].Day
		}
		return out[i].Tenant < out[j].Tenant
	})
	return out
}

// Prune removes usage older than keep days.
func (m *Meter) Prune(keep int) {
	cut := m.Now().UTC().AddDate(0, 0, -keep).Format("2006-01-02")
	for _, d := range m.Range("", "", cut) {
		if d.Day < cut {
			c, cancel := ctx5()
			_ = m.DB.Delete(c, coll, d.ID)
			cancel()
		}
	}
}

// csvSafe stops a spreadsheet from running a name that starts like a formula.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		s = "'" + s
	}
	if strings.ContainsAny(s, ",\"\n\r") {
		s = `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

// CSV writes days as a spreadsheet.
func CSV(days []Day) string {
	var b strings.Builder
	b.WriteString("tenant,day,items_total,bytes_total,traces_items,traces_bytes,logs_items,logs_bytes,metrics_items,metrics_bytes,hosts_reporting,peak_hosts,peak_instances,peak_users,peak_groups,peak_keys,peak_dashboards\n")
	for _, d := range days {
		fmt.Fprintf(&b, "%s,%s,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d\n", csvSafe(d.Tenant), d.Day, d.Items(), d.Bytes(), d.Traces.Items, d.Traces.Bytes, d.Logs.Items, d.Logs.Bytes, d.Metrics.Items, d.Metrics.Bytes,
			len(d.Hosts), d.Peak["hosts"], d.Peak["instances"], d.Peak["users"], d.Peak["groups"], d.Peak["keys"], d.Peak["dashboards"])
	}
	return b.String()
}

// ---- rate limit ----

// Limiter is a token bucket per tenant: it allows bursts of ten seconds at the tenant's rate, and a batch larger than
// that when the bucket is full (otherwise one big request could never get through).
type Limiter struct {
	Now func() time.Time
	mu  sync.Mutex
	b   map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewLimiter() *Limiter { return &Limiter{Now: time.Now, b: map[string]*bucket{}} }

// Allow takes n items from the tenant's bucket. If it cannot, it says how long to wait.
func (l *Limiter) Allow(tenant string, n, perSec int) (bool, time.Duration) {
	if perSec <= 0 || n <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now, capacity := l.Now(), float64(perSec)*10
	b := l.b[tenant]
	if b == nil {
		b = &bucket{tokens: capacity, last: now}
		l.b[tenant] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * float64(perSec)
	if b.tokens > capacity {
		b.tokens = capacity
	}
	b.last = now
	need := float64(n)
	if need > capacity {
		if b.tokens >= capacity { // a batch bigger than the burst: allowed when the bucket is full, and it empties it
			b.tokens = 0
			return true, 0
		}
		need = capacity
	}
	if b.tokens >= need {
		b.tokens -= need
		return true, 0
	}
	wait := time.Duration((need - b.tokens) / float64(perSec) * float64(time.Second))
	if wait < time.Second {
		wait = time.Second
	}
	return false, wait
}

// MarkChecker notes that an agent only checks instances: it is not a host, so it is not counted as one (and if it was
// counted before it said so, it no longer is).
func (m *Meter) MarkChecker(tenant, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.check[tenant] == nil {
		m.check[tenant] = map[string]bool{}
	}
	m.check[tenant][name] = true
	delete(m.hostsLocked(tenant), name)
}

// IsChecker tells whether an agent was said to only check instances.
func (m *Meter) IsChecker(tenant, name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.check[tenant][name]
}
