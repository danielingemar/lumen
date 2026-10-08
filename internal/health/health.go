// Package health lets Lumen look at itself: the disk it writes to, the state of Elasticsearch and ClickHouse, and what takes
// the space. It is there so that an administrator is told when a disk reaches 80 percent, not when the cluster has turned red.
package health

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Disk is a file system with how much room it has. It is the file system that holds the path, which for a container is the
// disk of the machine under the volume.
type Disk struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}

// UsedPct is how full the disk is, 0-100.
func (d Disk) UsedPct() float64 {
	if d.Total == 0 {
		return 0
	}
	return 100 * (1 - float64(d.Free)/float64(d.Total))
}

// ESNode is what Elasticsearch says about the disk of one of its nodes.
type ESNode struct {
	Name        string  `json:"name"`
	DiskPercent float64 `json:"disk_percent"`
	Avail       uint64  `json:"avail"`
	Total       uint64  `json:"total"`
}

// ES is the state of the Elasticsearch cluster that holds the settings.
type ES struct {
	Status     string `json:"status"` // green | yellow | red
	Unassigned int    `json:"unassigned_shards"`
	// the indices that have shards without a place, as "name (replica)" or "name (primary)"; empty when there are none
	UnassignedIndices []string `json:"unassigned_indices,omitempty"`
	Nodes             []ESNode `json:"nodes"`
	Err               string   `json:"error,omitempty"` // set when it could not be asked
}

// Table is a table of the telemetry store and how much disk it uses.
type Table struct {
	Name  string `json:"name"`
	Bytes uint64 `json:"bytes"`
}

// CH is the state of ClickHouse.
type CH struct {
	Disks  []Disk  `json:"disks"`
	Tables []Table `json:"tables"` // the biggest, system tables included
	Err    string  `json:"error,omitempty"`
}

// Report is everything at one moment.
type Report struct {
	At       time.Time `json:"at"`
	Disks    []Disk    `json:"disks"` // where Lumen keeps its own files
	ES       *ES       `json:"elasticsearch,omitempty"`
	CH       *CH       `json:"clickhouse,omitempty"`
	Problems []Problem `json:"problems"`
	Overall  string    `json:"overall"` // ok | warning | critical
}

// Problem is something to do something about, in words.
type Problem struct {
	Component string `json:"component"` // disk | elasticsearch | clickhouse
	Subject   string `json:"subject"`
	Level     string `json:"level"` // warning | critical
	Text      string `json:"text"`
	Advice    string `json:"advice"`
}

// Size writes bytes as a person reads them.
func Size(n uint64) string {
	const k = 1024
	if n < k {
		return fmt.Sprintf("%d B", n)
	}
	f, u := float64(n), []string{"KB", "MB", "GB", "TB", "PB"}
	i := -1
	for f >= k && i < len(u)-1 {
		f /= k
		i++
	}
	return fmt.Sprintf("%.3g %s", f, u[i])
}

const diskAdvice = "Elasticsearch stops creating indices at 85 percent and refuses writes at 95; ClickHouse needs room to merge and goes read-only when the disk is full. Free space (docker builder prune -af is safe), shorten the retention, or make the disk bigger."

// View is a disk as one of the parts sees it.
type View struct {
	Who  string
	Disk Disk
}

// Merged is one disk, with every part that sees it. Lumen's files, Elasticsearch and ClickHouse usually live on the same
// disk, and it must be one problem, not three.
type Merged struct {
	Who  []string
	Disk Disk
}

// MergeDisks puts the views that are the same disk together: the same size and the same room left, give or take half a gigabyte
// (each part measures at a slightly different moment).
func MergeDisks(views []View) []Merged {
	const g = 1 << 30
	key := func(d Disk) [2]int64 { return [2]int64{int64((d.Total + g/2) / g), int64((d.Free + g/2) / g)} }
	var out []Merged
	idx := map[[2]int64]int{}
	for _, v := range views {
		if v.Disk.Total == 0 {
			continue
		}
		k := key(v.Disk)
		if i, ok := idx[k]; ok {
			out[i].Who = append(out[i].Who, v.Who)
			continue
		}
		idx[k] = len(out)
		out = append(out, Merged{Who: []string{v.Who}, Disk: v.Disk})
	}
	return out
}

func joinWho(w []string) string {
	switch len(w) {
	case 1:
		return w[0]
	case 2:
		return w[0] + " and " + w[1]
	}
	return strings.Join(w[:len(w)-1], ", ") + " and " + w[len(w)-1]
}

// Views lists every disk the report knows of, each as the part that measured it sees it.
func Views(r Report) []View {
	var v []View
	for _, d := range r.Disks {
		v = append(v, View{"Lumen's " + d.Name + " (" + d.Path + ")", d})
	}
	if r.ES != nil {
		for _, n := range r.ES.Nodes {
			v = append(v, View{"Elasticsearch (node " + n.Name + ")", Disk{Total: n.Total, Free: n.Avail}})
		}
	}
	if r.CH != nil {
		for _, d := range r.CH.Disks {
			v = append(v, View{"ClickHouse (disk " + d.Name + ")", d})
		}
	}
	return v
}

func diskProblem(m Merged, warn, crit float64) *Problem {
	d := m.Disk
	u := d.UsedPct()
	if d.Total == 0 || u < warn {
		return nil
	}
	p := &Problem{Component: "disk", Subject: m.Who[0], Level: "warning", Advice: diskAdvice,
		Text: fmt.Sprintf("The disk used by %s is %.0f%% full (%s free of %s).", joinWho(m.Who), u, Size(d.Free), Size(d.Total))}
	if u >= crit {
		p.Level = "critical"
	}
	return p
}

// Evaluate turns a report into problems. warn and crit are percentages of a full disk.
func Evaluate(r Report, warn, crit float64) []Problem {
	var out []Problem
	for _, m := range MergeDisks(Views(r)) {
		if p := diskProblem(m, warn, crit); p != nil {
			out = append(out, *p)
		}
	}
	if r.ES != nil {
		switch {
		case r.ES.Err != "":
			out = append(out, Problem{Component: "elasticsearch", Subject: "elasticsearch", Level: "critical", Text: "Elasticsearch cannot be reached: " + r.ES.Err + ". Settings, users and dashboards are unavailable until it is back.", Advice: "docker compose logs elasticsearch, and look at the disk first."})
		case r.ES.Status == "red":
			out = append(out, Problem{Component: "elasticsearch", Subject: "elasticsearch", Level: "critical", Text: "Elasticsearch is red: some of its data cannot be read or written." + unassignedText(r.ES), Advice: "Most often the disk is over its limit, so a new index got no place. Free space first; then check the shards with GET /_cluster/allocation/explain."})
		case r.ES.Status == "yellow":
			out = append(out, Problem{Component: "elasticsearch", Subject: "elasticsearch", Level: "warning", Text: fmt.Sprintf("Elasticsearch is yellow: %d shard(s) have no place to live.", r.ES.Unassigned) + unassignedText(r.ES), Advice: yellowAdvice(r.ES)})
		}
	}
	if r.CH != nil && r.CH.Err != "" {
		out = append(out, Problem{Component: "clickhouse", Subject: "clickhouse", Level: "critical", Text: "ClickHouse cannot be reached: " + r.CH.Err + ". Traces, logs and metrics cannot be stored or read.", Advice: "docker compose logs clickhouse, and look at the disk first."})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Level == "critical" && out[j].Level != "critical" })
	return out
}

func unassignedText(e *ES) string {
	if len(e.UnassignedIndices) == 0 {
		return ""
	}
	return " Without a place: " + strings.Join(e.UnassignedIndices, ", ") + "."
}

// yellowAdvice says what yellow means here. A shard without a place that is a copy (a replica) needs a second node to live
// on, and on a single node it never gets one: harmless, but it keeps the cluster yellow.
func yellowAdvice(e *ES) string {
	if len(e.UnassignedIndices) > 0 {
		replicas := true
		for _, x := range e.UnassignedIndices {
			if !strings.HasSuffix(x, "(replica)") {
				replicas = false
			}
		}
		if replicas && len(e.Nodes) <= 1 {
			return "These are copies (replicas) that wait for a second node, which a single-node cluster does not have. Nothing is lost. To make it green: PUT /INDEX/_settings with {\"index\":{\"number_of_replicas\":0}} (Lumen does this for its own indices when it starts)."
		}
	}
	return "Check why with GET /_cluster/allocation/explain; a full disk is the usual reason."
}

// Overall is the worst level of a set of problems.
func Overall(ps []Problem) string {
	o := "ok"
	for _, p := range ps {
		if p.Level == "critical" {
			return "critical"
		}
		o = "warning"
	}
	return o
}

// Sources are where a Monitor looks. Any of them may be nil: Elasticsearch is not used by every installation.
type Sources struct {
	Paths   map[string]string        // name -> a path whose file system is measured
	PathsFn func() map[string]string // when set, asked each time (the backup folder can be changed in Settings); added to Paths
	ES      interface {
		Health(ctx context.Context) (ES, error)
	}
	CH interface {
		Health(ctx context.Context) (CH, error)
	}
}

// Monitor collects reports and remembers the last one for a while, so that a page that is opened often does not ask the
// databases every time.
type Monitor struct {
	Src        Sources
	Warn, Crit float64
	TTL        time.Duration
	Now        func() time.Time
	Statfs     func(path string) (total, free uint64, err error)

	mu   sync.Mutex
	last Report
	have bool
}

// Statfs measures the file system that holds a path.
var Statfs = statfs

func New(src Sources, warn, crit float64) *Monitor {
	if warn <= 0 || warn >= 100 {
		warn = 80
	}
	if crit <= warn || crit > 100 {
		crit = 90
	}
	return &Monitor{Src: src, Warn: warn, Crit: crit, TTL: 30 * time.Second, Now: time.Now, Statfs: statfs}
}

// Report returns the last report if it is recent, else makes a new one.
func (m *Monitor) Report(ctx context.Context) Report {
	m.mu.Lock()
	if m.have && m.Now().Sub(m.last.At) < m.TTL {
		r := m.last
		m.mu.Unlock()
		return r
	}
	m.mu.Unlock()
	return m.Refresh(ctx)
}

// Refresh always looks.
func (m *Monitor) Refresh(ctx context.Context) Report {
	r := Report{At: m.Now(), Disks: []Disk{}, Problems: []Problem{}}
	paths := map[string]string{}
	for n, p := range m.Src.Paths {
		paths[n] = p
	}
	if m.Src.PathsFn != nil {
		for n, p := range m.Src.PathsFn() {
			paths[n] = p
		}
	}
	names := make([]string, 0, len(paths))
	for n := range paths {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if total, free, err := m.Statfs(paths[n]); err == nil {
			r.Disks = append(r.Disks, Disk{Name: n, Path: paths[n], Total: total, Free: free})
		}
	}
	// the file system under /data and under the backups is often the same one: say it once, with both names
	byFS := map[[2]uint64]int{}
	uniq := r.Disks[:0]
	for _, d := range r.Disks {
		k := [2]uint64{d.Total, d.Free}
		if i, ok := byFS[k]; ok {
			uniq[i].Name += " and " + d.Name
			uniq[i].Path += ", " + d.Path
			continue
		}
		byFS[k] = len(uniq)
		uniq = append(uniq, d)
	}
	r.Disks = uniq
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if m.Src.ES != nil {
		es, err := m.Src.ES.Health(cctx)
		if err != nil {
			es.Err = reason(err)
		}
		r.ES = &es
	}
	if m.Src.CH != nil {
		ch, err := m.Src.CH.Health(cctx)
		if err != nil {
			ch.Err = reason(err)
		}
		if ch.Disks == nil {
			ch.Disks = []Disk{}
		}
		if ch.Tables == nil {
			ch.Tables = []Table{}
		}
		r.CH = &ch
	}
	r.Problems = Evaluate(r, m.Warn, m.Crit)
	r.Overall = Overall(r.Problems)
	m.mu.Lock()
	m.last, m.have = r, true
	m.mu.Unlock()
	return r
}

// reason is what went wrong, without the address that was being asked (a Go network error starts with the whole URL).
func reason(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return err.Error()
}
