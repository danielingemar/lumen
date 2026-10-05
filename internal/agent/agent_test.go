package agent

import (
	"compress/gzip"
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/otlp"
)

const promText = `# HELP http_requests_total Total requests
# TYPE http_requests_total counter
http_requests_total{method="get",path="/a,b \"x\"",code="200"} 1027 1395066363000
http_requests_total{method="post",code="500"} 3
# TYPE temp gauge
temp 21.5
bad_nan NaN
up{job="x"} 1
`

func TestParseProm(t *testing.T) {
	pts := ParseProm(strings.NewReader(promText), "svc", 42)
	if len(pts) != 4 { // NaN skipped
		t.Fatalf("want 4 points, got %d: %+v", len(pts), pts)
	}
	if p := pts[0]; p.Name != "http_requests_total" || p.Type != "sum" || p.Value != 1027 || p.Attrs["path"] != `/a,b "x"` || p.Attrs["code"] != "200" {
		t.Fatalf("bad first point: %+v", p)
	}
	if pts[2].Type != "gauge" || pts[2].Value != 21.5 || pts[2].Attrs != nil && len(pts[2].Attrs) != 0 {
		t.Fatalf("bad gauge: %+v", pts[2])
	}
}

func TestPayloadsRoundTripThroughLumenDecoder(t *testing.T) {
	m, _ := MetricsPayload("svc", "h1", []Point{
		{Service: "svc", Name: "cpu", Type: "gauge", Value: 0.5, Attrs: map[string]string{"core": "0"}, TimeNs: 1700000000000000000},
		{Service: "svc", Name: "reqs_total", Type: "sum", Value: 9, TimeNs: 1700000000000000000},
	})
	pts, err := otlp.DecodeMetrics(m, "acme")
	if err != nil || len(pts) != 2 {
		t.Fatalf("metrics decode: %v %+v", err, pts)
	}
	for _, p := range pts {
		if p.Service != "svc" || p.Tenant != "acme" {
			t.Fatalf("bad point %+v", p)
		}
	}
	l, _ := LogsPayload("svc", "h1", []Log{{TimeNs: 1700000000000000000, Body: "hello", Severity: "ERROR", Attrs: map[string]string{"a": "b"}}})
	logs, err := otlp.DecodeLogs(l, "acme")
	if err != nil || len(logs) != 1 || logs[0].Body != "hello" || logs[0].Severity != "ERROR" || logs[0].Service != "svc" {
		t.Fatalf("logs decode: %v %+v", err, logs)
	}
}

func TestTailer(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "app.log")
	os.WriteFile(f, []byte("old line\n"), 0o644)
	tl := &Tailer{Patterns: []string{filepath.Join(dir, "*.log")}, Service: "s"}
	if got := tl.Poll(); len(got) != 0 {
		t.Fatalf("history must not be backfilled, got %v", got)
	}
	fh, _ := os.OpenFile(f, os.O_APPEND|os.O_WRONLY, 0o644)
	fh.WriteString("new1\npartial")
	if got := tl.Poll(); len(got) != 1 || got[0].Body != "new1" {
		t.Fatalf("want [new1], got %+v", got)
	}
	fh.WriteString(" done\n")
	if got := tl.Poll(); len(got) != 1 || got[0].Body != "partial done" {
		t.Fatalf("partial line not completed: %+v", got)
	}
	os.WriteFile(f, []byte("after rotate\n"), 0o644) // truncation
	if got := tl.Poll(); len(got) != 1 || got[0].Body != "after rotate" {
		t.Fatalf("truncation not handled: %+v", got)
	}
	os.WriteFile(filepath.Join(dir, "b.log"), []byte("fresh file\n"), 0o644) // new file: read from start
	if got := tl.Poll(); len(got) != 1 || got[0].Body != "fresh file" {
		t.Fatalf("new file: %+v", got)
	}
}

func TestDockerFormat(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c-json.log")
	os.WriteFile(f, nil, 0o644)
	tl := &Tailer{Patterns: []string{f}, Service: "s", Format: "docker"}
	tl.Poll()
	fh, _ := os.OpenFile(f, os.O_APPEND|os.O_WRONLY, 0o644)
	fh.WriteString(`{"log":"hello world\n","stream":"stderr","time":"2026-01-02T03:04:05.123456789Z"}` + "\n")
	got := tl.Poll()
	if len(got) != 1 || got[0].Body != "hello world" || got[0].Attrs["log.iostream"] != "stderr" || got[0].TimeNs != time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC).UnixNano() {
		t.Fatalf("docker parse: %+v", got)
	}
}

func TestSenderRetriesAndAuth(t *testing.T) {
	var calls int32
	var gotBody, gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(503)
			return
		}
		gz, _ := gzip.NewReader(r.Body)
		b, _ := io.ReadAll(gz)
		gotBody, gotAuth = string(b), r.Header.Get("Authorization")
		w.Write([]byte("{}"))
	}))
	defer ts.Close()
	s := &Sender{URL: ts.URL, APIKey: "k", Retry: time.Millisecond}
	if err := s.Post(context.Background(), "/v1/metrics", []byte(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || gotBody != `{"x":1}` || gotAuth != "Bearer k" {
		t.Fatalf("calls=%d body=%q auth=%q", calls, gotBody, gotAuth)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer bad.Close()
	calls = 0
	if err := (&Sender{URL: bad.URL, Retry: time.Millisecond}).Post(context.Background(), "/x", []byte("{}")); err == nil {
		t.Fatal("401 must fail without retry")
	}
}

func TestHostCollectSmoke(t *testing.T) {
	h := &Host{Service: "host"}
	h.Collect(1)
	pts := h.Collect(2)
	if _, err := os.Stat("/proc/stat"); err == nil && len(pts) == 0 {
		t.Fatal("expected host metrics on linux")
	}
}

func TestSelfMetrics(t *testing.T) {
	self := NewSelf("h1")
	self.Sent("metrics", 10)
	self.Sent("metrics", 5)
	self.Sent("logs", 3)
	self.SendError("logs")
	self.ObserveScrape("http://user:secret@node:9100/metrics?token=abc", nil)
	self.ObserveScrape("http://user:secret@node:9100/metrics?token=abc", io.EOF)

	get := func(name string, kv ...string) (Point, bool) {
	outer:
		for _, p := range self.Collect(SelfService, 1) {
			if p.Name != name {
				continue
			}
			for i := 0; i < len(kv); i += 2 {
				if p.Attrs[kv[i]] != kv[i+1] {
					continue outer
				}
			}
			return p, true
		}
		return Point{}, false
	}
	if p, ok := get("lumen_agent_items_sent_total", "signal", "metrics"); !ok || p.Value != 15 || p.Type != "sum" {
		t.Fatalf("sent metrics: %+v %v", p, ok)
	}
	if p, ok := get("lumen_agent_send_errors_total", "signal", "logs"); !ok || p.Value != 1 {
		t.Fatalf("send errors: %+v", p)
	}
	if p, ok := get("lumen_agent_scrape_errors_total", "target", "node:9100/metrics"); !ok || p.Value != 1 {
		t.Fatalf("scrape errors (credentials must be stripped from target): %+v", p)
	}
	if p, ok := get("lumen_agent_scrapes_total", "target", "node:9100/metrics"); !ok || p.Value != 2 {
		t.Fatalf("scrapes: %+v", p)
	}
	if _, ok := get("lumen_agent_uptime_seconds"); !ok {
		t.Fatal("uptime missing")
	}
}

// The agent's /metrics output must be readable by the agent's own scraper (and so by Prometheus).
func TestSelfEndpointIsScrapable(t *testing.T) {
	self := NewSelf("h1")
	self.Sent("metrics", 7)
	ts := httptest.NewServer(self.Handler())
	defer ts.Close()
	pts, err := Scrape(context.Background(), ts.URL+"/metrics", "self-scrape")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range pts {
		if p.Name == "lumen_agent_items_sent_total" && p.Attrs["signal"] == "metrics" && p.Value == 7 && p.Type == "sum" {
			found = true
		}
	}
	if !found {
		t.Fatalf("self metric not found in scrape: %+v", pts)
	}
}

// On a Linux machine the whole collector runs against the real /proc: names stay as before, new ones appear, and
// the values are plausible.
func TestHostCollectRealProc(t *testing.T) {
	if _, err := os.Stat("/proc/stat"); err != nil {
		t.Skip("no /proc")
	}
	h := &Host{Service: "host"}
	h.Collect(1_000_000_000)
	time.Sleep(300 * time.Millisecond)
	// burn a little CPU and touch the disk so there is something to measure
	x := 0
	for i := 0; i < 5_000_000; i++ {
		x += i
	}
	_ = x
	pts := h.Collect(1_000_000_000 + 300_000_000)
	by := map[string][]Point{}
	for _, p := range pts {
		by[p.Name] = append(by[p.Name], p)
		if p.Value < 0 || math.IsNaN(p.Value) || math.IsInf(p.Value, 0) {
			t.Errorf("implausible value: %+v", p)
		}
	}
	for _, n := range []string{"system.cpu.utilization", "system.cpu.state", "system.cpu.count", "system.memory.total", "system.memory.used", "system.memory.usage", "system.load.1m", "system.load.average", "system.uptime", "system.processes.count", "system.paging.usage"} {
		if len(by[n]) == 0 {
			t.Errorf("missing %s", n)
		}
	}
	if u := by["system.cpu.utilization"]; len(u) == 1 && (u[0].Value < 0 || u[0].Value > 1) {
		t.Errorf("cpu utilization out of range: %v", u[0].Value)
	}
	sum := 0.0
	for _, p := range by["system.cpu.state"] {
		sum += p.Value
	}
	if len(by["system.cpu.state"]) != 6 || sum > 1.0001 {
		t.Errorf("cpu states: %d values summing to %v", len(by["system.cpu.state"]), sum)
	}
	mem := 0.0
	for _, p := range by["system.memory.usage"] {
		mem += p.Value
	}
	if tot := by["system.memory.total"]; len(tot) == 1 && math.Abs(mem-tot[0].Value) > tot[0].Value*0.001 {
		t.Errorf("memory states %v must add up to the total %v", mem, tot[0].Value)
	}
	for _, p := range by["system.filesystem.used"] {
		if p.Attrs["mountpoint"] == "" || p.Attrs["device"] == "" || p.Attrs["fstype"] == "" {
			t.Errorf("filesystem labels: %+v", p.Attrs)
		}
	}
	for _, p := range by["system.filesystem.utilization"] {
		if p.Value > 1 {
			t.Errorf("filesystem utilization above 100%%: %+v", p)
		}
	}
	for _, p := range by["system.disk.io.bytes_rate"] {
		if d := p.Attrs["direction"]; d != "read" && d != "write" || p.Attrs["device"] == "" {
			t.Errorf("disk io labels: %+v", p.Attrs)
		}
	}
	for _, p := range by["system.network.io.bytes_rate"] {
		if p.Attrs["device"] == "lo" || strings.HasPrefix(p.Attrs["device"], "veth") || strings.HasPrefix(p.Attrs["device"], "docker") {
			t.Errorf("virtual interface reported: %+v", p.Attrs)
		}
	}
	t.Logf("%d points; names: %d; mounts: %d; disks: %d; interfaces: %d", len(pts), len(by), len(by["system.filesystem.total"]), len(by["system.disk.utilization"]), len(by["system.network.io.bytes_rate"])/2)
}
