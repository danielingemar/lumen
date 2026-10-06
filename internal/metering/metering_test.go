package metering

import (
	"strings"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
)

var t0 = time.Date(2026, 10, 6, 23, 30, 0, 0, time.UTC)

func newMeter(t *testing.T) (*Meter, docstore.Backend, *time.Time) {
	db, _ := docstore.OpenFile(t.TempDir())
	now := t0
	m := New(db)
	m.Now = func() time.Time { return now }
	return m, db, &now
}

func TestCountsPerTenantSignalAndDay(t *testing.T) {
	m, db, now := newMeter(t)
	m.Record("acme", "metrics", 100, 5000, []string{"web1", "web2", "web1"})
	m.Record("acme", "logs", 40, 2000, nil)
	m.Record("acme", "traces", 7, 700, nil)
	m.Record("globex", "metrics", 1, 10, []string{"db1"})
	m.Record("acme", "weird", 99, 99, nil) // not a signal: ignored
	d := m.Today("acme")
	if d.Metrics.Items != 100 || d.Logs.Bytes != 2000 || d.Traces.Items != 7 || d.Items() != 147 || d.Bytes() != 7700 || len(d.Hosts) != 2 || d.Day != "2026-10-06" {
		t.Fatalf("%+v", d)
	}
	if g := m.Today("globex"); g.Items() != 1 || len(g.Hosts) != 1 {
		t.Fatalf("one tenant's numbers never mix with another's: %+v", g)
	}
	// midnight: a new day starts at zero and yesterday stays as it was
	*now = time.Date(2026, 10, 7, 0, 5, 0, 0, time.UTC)
	m.Record("acme", "metrics", 5, 50, []string{"web1"})
	if d := m.Today("acme"); d.Items() != 5 || d.Day != "2026-10-07" {
		t.Fatalf("%+v", d)
	}
	m.Flush()
	days := m.Range("acme", "", "")
	if len(days) != 2 || days[0].Items() != 147 || days[1].Items() != 5 {
		t.Fatalf("%+v", days)
	}
	// a restart carries on counting where it stopped
	m2 := New(db)
	m2.Now = m.Now
	m2.Record("acme", "metrics", 10, 100, []string{"web3"})
	if d := m2.Today("acme"); d.Items() != 15 || len(d.Hosts) != 2 { // web1 and web3
		t.Fatalf("counting continues after a restart: %+v", d)
	}
	if got := m.Range("acme", "2026-10-07", "2026-10-07"); len(got) != 1 {
		t.Fatalf("range is inclusive on both ends: %v", got)
	}
	if got := m.Range("", "", ""); len(got) != 3 {
		t.Fatalf("all tenants: %d", len(got))
	}
}

func TestHostsAreKnownAcrossRestartsAndExpireAfter24Hours(t *testing.T) {
	m, db, now := newMeter(t)
	m.Record("acme", "metrics", 1, 1, []string{"web1", "web2"})
	m.Flush()
	m2 := New(db)
	m2.Now = m.Now
	if m2.ActiveHosts("acme") != 2 || m2.NewHosts("acme", []string{"web1", "web9"}) != 1 {
		t.Fatalf("after a restart the hosts are still known (so a limit does not turn known hosts away): %d", m2.ActiveHosts("acme"))
	}
	*now = t0.Add(25 * time.Hour)
	if m2.ActiveHosts("acme") != 0 || m2.NewHosts("acme", []string{"web1"}) != 1 {
		t.Fatal("a host that has been silent for 24 hours does not count")
	}
	if m2.NewHosts("acme", []string{"a", "a", "", "b"}) != 2 {
		t.Fatal("duplicates and blanks are not counted")
	}
	m2.Record("acme", "metrics", 1, 1, []string{strings.Repeat("h", 300), ""})
	if len(m2.Today("acme").Hosts) != 0 {
		t.Fatal("absurd or empty host names are ignored")
	}
}

func TestPeaksAndPrune(t *testing.T) {
	m, _, now := newMeter(t)
	m.Sample("acme", map[string]int{"users": 3, "keys": 1})
	m.Sample("acme", map[string]int{"users": 5})
	m.Sample("acme", map[string]int{"users": 4})
	if d := m.Today("acme"); d.Peak["users"] != 5 || d.Peak["keys"] != 1 {
		t.Fatalf("the highest of the day is kept: %v", d.Peak)
	}
	*now = t0.AddDate(0, 0, 40)
	m.Record("acme", "logs", 1, 1, nil)
	m.Flush()
	m.Prune(30)
	if got := m.Range("acme", "", ""); len(got) != 1 || got[0].Day != "2026-11-15" {
		t.Fatalf("usage older than the limit is removed: %+v", got)
	}
}

func TestCSV(t *testing.T) {
	m, _, _ := newMeter(t)
	m.Record("acme", "metrics", 100, 5000, []string{"web1"})
	m.Sample("acme", map[string]int{"hosts": 4, "users": 2})
	m.Record("=cmd|' /C calc'!A0", "logs", 1, 1, nil)
	m.Record("a,b", "logs", 1, 1, nil)
	out := CSV(m.Range("", "", ""))
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.HasPrefix(lines[0], "tenant,day,items_total,bytes_total") || len(lines) != 4 {
		t.Fatalf("%s", out)
	}
	var acme string
	for _, l := range lines {
		if strings.HasPrefix(l, "acme,") {
			acme = l
		}
	}
	if acme != "acme,2026-10-06,100,5000,0,0,0,0,100,5000,1,4,0,2,0,0,0" {
		t.Fatalf("%s", acme)
	}
	if strings.Contains(out, "\n=cmd") || !strings.Contains(out, "'=cmd") || !strings.Contains(out, `"a,b"`) {
		t.Fatalf("a spreadsheet must not run a tenant name as a formula, and commas are quoted:\n%s", out)
	}
}

func TestLimiter(t *testing.T) {
	now := t0
	l := NewLimiter()
	l.Now = func() time.Time { return now }
	// 10 items per second: a burst of 100 is allowed, then it is refilled at 10 per second
	if ok, _ := l.Allow("acme", 60, 10); !ok {
		t.Fatal("burst")
	}
	if ok, _ := l.Allow("acme", 40, 10); !ok {
		t.Fatal("up to ten seconds' worth at once")
	}
	ok, wait := l.Allow("acme", 30, 10)
	if ok || wait != 3*time.Second {
		t.Fatalf("empty: wait 3 s for 30 more items at 10 per second: %v %v", ok, wait)
	}
	now = now.Add(3 * time.Second)
	if ok, _ := l.Allow("acme", 30, 10); !ok {
		t.Fatal("refilled")
	}
	// another tenant has its own bucket
	if ok, _ := l.Allow("globex", 100, 10); !ok {
		t.Fatal("tenants do not share a bucket")
	}
	// no limit
	if ok, _ := l.Allow("acme", 1_000_000, 0); !ok {
		t.Fatal("0 = no limit")
	}
	// a batch bigger than the burst gets through when the bucket is full, once
	now = now.Add(time.Hour)
	if ok, _ := l.Allow("acme", 5000, 10); !ok {
		t.Fatal("a big batch must be able to get through when the tenant has been quiet")
	}
	if ok, wait := l.Allow("acme", 5000, 10); ok || wait < time.Second {
		t.Fatalf("but not twice in a row: %v %v", ok, wait)
	}
	now = now.Add(5 * time.Second)
	if ok, _ := l.Allow("acme", 5000, 10); ok {
		t.Fatal("half full is not full")
	}
	now = now.Add(10 * time.Second)
	if ok, _ := l.Allow("acme", 5000, 10); !ok {
		t.Fatal("full again after ten seconds")
	}
}
