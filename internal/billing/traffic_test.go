package billing

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/inmagma/lumen/internal/docstore"
)

type trafficSource struct {
	fakeSource
	traffic []TrafficSample
}

func (s *trafficSource) ContainerTraffic(ctx context.Context, tenant string, from, to time.Time) ([]TrafficSample, error) {
	if tenant != "acme" {
		return nil, nil
	}
	return s.traffic, nil
}

func tdays(from, to int, sent, recv float64) map[string]TrafficDay {
	m := map[string]TrafficDay{}
	for d := from; d <= to; d++ {
		m[time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC).Format("2006-01-02")] = TrafficDay{Sent: sent, Received: recv}
	}
	return m
}

func TestTrafficIsChargedInsteadOfStoredData(t *testing.T) {
	cfg := Config{Currency: "EUR", UserPrice: "10", GBPrice: "0.10", DataMode: Traffic, Profiles: []Profile{{Key: "kund.example.com", Containers: []string{"nextcloud-ks", "web-ks"}}}}
	months := []Month{{Instance: "kund.example.com", Month: "2026-10", Days: days(1, 31, 5, 900)}} // 900 GB stored: not what is charged
	// the app sends 10 GB a day and the web server 5 GB; a container of another customer is not counted
	traffic := []TrafficMonth{
		{Host: "h1", Container: "nextcloud-ks", Month: "2026-10", Days: tdays(1, 31, 10e9, 1e9)},
		{Host: "h1", Container: "web-ks", Month: "2026-10", Days: tdays(1, 31, 5e9, 0)},
		{Host: "h1", Container: "nextcloud-ki", Month: "2026-10", Days: tdays(1, 31, 99e9, 0)},
	}
	r, err := BuildRangeT(cfg, "2026-10-01", "2026-10-31", months, traffic, nil, oct)
	if err != nil {
		t.Fatal(err)
	}
	l := r.Invoices[0].Lines[0]
	// 31 days × 15 GB = 465 GB sent × 0.10 = 46.50, and 5 users × 10 = 50.00
	if r.DataMode != Traffic || l.GB != 465 || l.DataAmount != 4650 || l.UserAmount != 5000 || len(l.Warnings) != 0 {
		t.Fatalf("%+v %v", l, r.Warnings)
	}
	// sent and received
	cfg.Traffic = Both
	r, _ = BuildRangeT(cfg, "2026-10-01", "2026-10-31", months, traffic, nil, oct)
	if l = r.Invoices[0].Lines[0]; l.GB != 496 || l.DataAmount != 4960 {
		t.Fatalf("%+v", l)
	}
	// only some of the days
	cfg.Traffic = Sent
	r, _ = BuildRangeT(cfg, "2026-10-10", "2026-10-19", months, traffic, nil, oct)
	if l = r.Invoices[0].Lines[0]; l.GB != 150 {
		t.Fatalf("ten days of 15 GB: %+v", l)
	}
	// the CSV says what the column is
	var b bytes.Buffer
	WriteCSV(&b, r, "lines", "")
	if !strings.Contains(b.String(), "traffic_gb,gb_price,traffic_amount") {
		t.Fatalf("%s", b.String())
	}
}

func TestTrafficWithoutContainersIsExplainedNotInvented(t *testing.T) {
	months := []Month{{Instance: "a.example.com", Month: "2026-10", Days: days(1, 31, 3, 1)}}
	cfg := Config{Currency: "EUR", GBPrice: "1", DataMode: Traffic}
	r, _ := BuildRangeT(cfg, "2026-10-01", "2026-10-31", months, nil, nil, oct)
	l := r.Invoices[0].Lines[0]
	if l.GB != 0 || l.DataAmount != 0 || len(l.Warnings) != 1 || !strings.Contains(l.Warnings[0], "No container is chosen") {
		t.Fatalf("%+v", l)
	}
	// a name that matches nothing that was recorded is told apart from a quiet container
	cfg.Profiles = []Profile{{Key: "a.example.com", Containers: []string{"nextcloud-typo"}}}
	r, _ = BuildRangeT(cfg, "2026-10-01", "2026-10-31", months, []TrafficMonth{{Host: "h", Container: "nextcloud", Month: "2026-10", Days: tdays(1, 5, 1e9, 0)}}, nil, oct)
	if w := r.Invoices[0].Lines[0].Warnings; len(w) != 1 || !strings.Contains(w[0], "No traffic was recorded") {
		t.Fatalf("%v", w)
	}
	// traffic is not shared out over the month, even when the days are
	cfg.Prorate = true
	cfg.Profiles = []Profile{{Key: "a.example.com", Containers: []string{"h/nextcloud"}}}
	months[0].Days = days(10, 19, 3, 1)
	r, _ = BuildRangeT(cfg, "2026-10-01", "2026-10-31", months, []TrafficMonth{{Host: "h", Container: "nextcloud", Month: "2026-10", Days: tdays(10, 19, 2e9, 0)}, {Host: "other", Container: "nextcloud", Month: "2026-10", Days: tdays(1, 31, 50e9, 0)}}, nil, oct)
	if l = r.Invoices[0].Lines[0]; l.GB != 20 || l.DataAmount != 2000 {
		t.Fatalf("host/name picks that machine only: %+v", l)
	}
}

func TestTrafficConfigIsChecked(t *testing.T) {
	c, err := Config{Currency: "EUR", Profiles: []Profile{{Key: "a", Containers: []string{" nextcloud-ks ", "", "nextcloud-ks", "host1/web"}}}}.Clean()
	if err != nil || c.DataMode != Stored || c.Traffic != Sent || len(c.Profiles[0].Containers) != 2 {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []Config{{DataMode: "bytes"}, {Traffic: "all"}, {Profiles: []Profile{{Key: "a", Containers: []string{"a b"}}}}, {Profiles: []Profile{{Key: "a", Containers: []string{"-x"}}}}, {Profiles: []Profile{{Key: "a", Containers: []string{"a/b/c"}}}}} {
		if _, err := bad.Clean(); err == nil {
			t.Errorf("%+v should be refused", bad)
		}
	}
}

func TestRecorderKeepsTheTrafficAndReplacesADayThatIsReadAgain(t *testing.T) {
	db, _ := docstore.OpenFile(t.TempDir())
	src := &trafficSource{traffic: []TrafficSample{
		{"h1", "nextcloud-ks", "2026-10-07", 5e9, 1e9},
		{"h1", "nextcloud-ks", "2026-10-08", 2e9, 0},
		{"h1", "", "2026-10-08", 9, 9},
	}}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	rec := &Recorder{DB: db, Src: src, Tenants: func() []string { return []string{"acme", "globex"} }, Now: func() time.Time { return now }, Backfill: 35 * 24 * time.Hour}
	rec.Pass(context.Background())
	from, to := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	tm, err := TrafficIn(context.Background(), db, "acme", from, to)
	if err != nil || len(tm) != 1 || tm[0].Days["2026-10-07"].Sent != 5e9 || tm[0].Days["2026-10-08"].Received != 0 {
		t.Fatalf("%+v %v", tm, err)
	}
	if g, _ := TrafficIn(context.Background(), db, "globex", from, to); len(g) != 0 {
		t.Fatal("another tenant has none")
	}
	src.traffic = []TrafficSample{{"h1", "nextcloud-ks", "2026-10-08", 3e9, 0}}
	now = now.Add(time.Hour)
	rec.Pass(context.Background())
	tm, _ = TrafficIn(context.Background(), db, "acme", from, to)
	if tm[0].Days["2026-10-08"].Sent != 3e9 || tm[0].Days["2026-10-07"].Sent != 5e9 {
		t.Fatalf("a day read again is replaced, the others stay: %+v", tm[0].Days)
	}
}
