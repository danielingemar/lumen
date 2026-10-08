package health

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

const gb = 1 << 30

var gbf = float64(gb) // for sizes with decimals

func disk(name string, usedPct float64) Disk {
	return Disk{Name: name, Path: "/" + name, Total: 100 * gb, Free: uint64(float64(100*gb) * (100 - usedPct) / 100)}
}

func TestDisksAreJudgedByHowFullTheyAre(t *testing.T) {
	for _, c := range []struct {
		used float64
		want string
	}{{50, ""}, {79.9, ""}, {80, "warning"}, {89, "warning"}, {90, "critical"}, {99, "critical"}} {
		ps := Evaluate(Report{Disks: []Disk{disk("data", c.used)}}, 80, 90)
		got := ""
		if len(ps) > 0 {
			got = ps[0].Level
		}
		if got != c.want {
			t.Errorf("%.1f%%: %q, want %q", c.used, got, c.want)
		}
	}
	ps := Evaluate(Report{Disks: []Disk{{Name: "data", Path: "/data", Total: 70 * gb, Free: uint64(4.9 * gbf)}}}, 80, 90)
	if len(ps) != 1 || !strings.Contains(ps[0].Text, "93% full") || !strings.Contains(ps[0].Text, "4.9 GB free of 70 GB") || !strings.Contains(ps[0].Text, "/data") || !strings.Contains(ps[0].Advice, "85 percent") {
		t.Fatalf("the problem says which disk, how full and how much is left, and what to do: %+v", ps)
	}
	if got := Evaluate(Report{Disks: []Disk{{Name: "x", Path: "/x"}}}, 80, 90); len(got) != 0 {
		t.Fatal("a disk that reports no size is not a problem")
	}
}

func TestElasticsearchAndClickHouse(t *testing.T) {
	r := Report{ES: &ES{Status: "green", Nodes: []ESNode{{Name: "n1", DiskPercent: 50, Avail: 50 * gb, Total: 100 * gb}}}, CH: &CH{Disks: []Disk{disk("default", 40)}}}
	if ps := Evaluate(r, 80, 90); len(ps) != 0 || Overall(ps) != "ok" {
		t.Fatalf("all well: %+v", ps)
	}
	r.ES.Status, r.ES.Unassigned = "yellow", 3
	ps := Evaluate(r, 80, 90)
	if len(ps) != 1 || ps[0].Level != "warning" || !strings.Contains(ps[0].Text, "yellow") || !strings.Contains(ps[0].Text, "3 shard") {
		t.Fatalf("%+v", ps)
	}
	r.ES.Status = "red"
	r.ES.Nodes[0] = ESNode{Name: "n1", Avail: 5 * gb, Total: 100 * gb}
	ps = Evaluate(r, 80, 90)
	if len(ps) != 2 || ps[0].Level != "critical" || Overall(ps) != "critical" || !strings.Contains(ps[0].Advice, "disk") {
		t.Fatalf("red comes first, and Elasticsearch's own view of its disk is a problem too: %+v", ps)
	}
	if !strings.Contains(ps[1].Text, "Elasticsearch's own view") || ps[1].Component != "elasticsearch" {
		t.Fatalf("%+v", ps[1])
	}
	r.ES = &ES{Err: "connection refused"}
	r.CH = &CH{Err: "dial tcp: connection refused"}
	ps = Evaluate(r, 80, 90)
	if len(ps) != 2 || ps[0].Level != "critical" || !strings.Contains(ps[0].Text, "cannot be reached") || !strings.Contains(ps[1].Text, "ClickHouse cannot be reached") {
		t.Fatalf("%+v", ps)
	}
	r.ES, r.CH = nil, &CH{Disks: []Disk{disk("default", 95)}}
	if ps = Evaluate(r, 80, 90); len(ps) != 1 || ps[0].Level != "critical" || ps[0].Component != "clickhouse" || !strings.Contains(ps[0].Text, "ClickHouse's disk default") {
		t.Fatalf("%+v", ps)
	}
	if Overall(nil) != "ok" || Overall([]Problem{{Level: "warning"}}) != "warning" {
		t.Fatal("overall")
	}
}

type fakeES struct {
	es  ES
	err error
	n   int
}

func (f *fakeES) Health(context.Context) (ES, error) { f.n++; return f.es, f.err }

type fakeCH struct {
	ch  CH
	err error
}

func (f *fakeCH) Health(context.Context) (CH, error) { return f.ch, f.err }

func TestTheMonitor(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	es := &fakeES{es: ES{Status: "green"}}
	m := New(Sources{Paths: map[string]string{"data": "/data", "backups": "/backup"}, ES: es, CH: &fakeCH{ch: CH{Disks: []Disk{disk("default", 10)}, Tables: []Table{{Name: "otel_logs", Bytes: 5 * gb}}}}}, 80, 90)
	m.Now = func() time.Time { return now }
	free := uint64(15 * gb) // 85 percent used
	m.Statfs = func(p string) (uint64, uint64, error) { return 100 * gb, free, nil }
	r := m.Report(context.Background())
	if r.Overall != "warning" || len(r.Disks) != 1 || r.Disks[0].Name != "backups and data" || r.Disks[0].Path != "/backup, /data" {
		t.Fatalf("both paths are on the same file system, which is shown once, with both names: %+v", r.Disks)
	}
	if len(r.Problems) != 1 || r.Problems[0].Level != "warning" {
		t.Fatalf("%+v", r.Problems)
	}
	// the answer is kept for a while
	free = 90 * gb
	if r2 := m.Report(context.Background()); r2.Overall != "warning" || es.n != 1 {
		t.Fatalf("a report that is recent is reused, so a page that is opened often does not ask the databases each time: %s %d", r2.Overall, es.n)
	}
	now = now.Add(31 * time.Second)
	if r3 := m.Report(context.Background()); r3.Overall != "ok" || es.n != 2 {
		t.Fatalf("and a new one is made when it is old: %s %d", r3.Overall, es.n)
	}
	// a database that cannot be asked is a problem, not a missing field
	es.err = errors.New("boom")
	r4 := m.Refresh(context.Background())
	if r4.ES == nil || r4.ES.Err != "boom" || r4.Overall != "critical" {
		t.Fatalf("%+v", r4.ES)
	}
	// without Elasticsearch (a small installation keeps its settings in files) there is nothing to report about it
	m2 := New(Sources{Paths: map[string]string{"data": "/data"}}, 0, 0)
	m2.Statfs = func(string) (uint64, uint64, error) { return 100 * gb, 50 * gb, nil }
	if r := m2.Refresh(context.Background()); r.ES != nil || r.CH != nil || r.Overall != "ok" || m2.Warn != 80 || m2.Crit != 90 {
		t.Fatalf("%+v warn=%v crit=%v", r, m2.Warn, m2.Crit)
	}
	// a path that cannot be measured is left out
	m3 := New(Sources{Paths: map[string]string{"data": "/data"}}, 70, 85)
	m3.Statfs = func(string) (uint64, uint64, error) { return 0, 0, errors.New("no such path") }
	if r := m3.Refresh(context.Background()); len(r.Disks) != 0 || m3.Warn != 70 {
		t.Fatalf("%+v", r)
	}
}

func TestSize(t *testing.T) {
	for in, want := range map[uint64]string{0: "0 B", 1023: "1023 B", 1024: "1 KB", 5 * gb: "5 GB", uint64(4.9 * gbf): "4.9 GB"} {
		if got := Size(in); got != want {
			t.Errorf("%d: %q want %q", in, got, want)
		}
	}
}

func TestAnErrorDoesNotCarryTheAddress(t *testing.T) {
	e := &url.Error{Op: "Post", URL: "http://clickhouse:8123?database=lumen&output_format_json_quote_64bit_integers=1", Err: errors.New("dial tcp 172.18.0.3:8123: connect: connection refused")}
	if got := reason(e); got != "dial tcp 172.18.0.3:8123: connect: connection refused" {
		t.Fatal(got)
	}
	if reason(errors.New("plain")) != "plain" {
		t.Fatal("other errors are kept")
	}
	m := New(Sources{CH: &fakeCH{err: e}}, 80, 90)
	m.Statfs = func(string) (uint64, uint64, error) { return 0, 0, errors.New("x") }
	if r := m.Refresh(context.Background()); r.CH == nil || strings.Contains(r.CH.Err, "http://") || !strings.Contains(r.CH.Err, "connection refused") {
		t.Fatalf("%+v", r.CH)
	}
}
