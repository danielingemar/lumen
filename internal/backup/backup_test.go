package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
)

// fakeSrc is a tiny in-memory telemetry database: rows[table][tenant][day] = lines.
type fakeSrc struct {
	mu      sync.Mutex
	rows    map[string]map[string]map[string][]string
	archive map[string][]string // "table/tenant/day" -> lines
	calls   []string
	failOn  string
}

func (f *fakeSrc) day(t time.Time) string { return t.UTC().Format("2006-01-02") }

func (f *fakeSrc) Tenants(_ context.Context, table string, day time.Time) ([]string, error) {
	var out []string
	for tenant, days := range f.rows[table] {
		if len(days[f.day(day)]) > 0 {
			out = append(out, tenant)
		}
	}
	return out, nil
}
func (f *fakeSrc) Export(_ context.Context, table, tenant string, day time.Time, w io.Writer) error {
	f.calls = append(f.calls, "export "+table+" "+tenant+" "+f.day(day))
	if f.failOn == table {
		return errors.New("clickhouse is down")
	}
	for _, l := range f.rows[table][tenant][f.day(day)] {
		fmt.Fprintln(w, l)
	}
	return nil
}
func (f *fakeSrc) Import(_ context.Context, table string, r io.Reader) error {
	b, _ := io.ReadAll(r)
	f.calls = append(f.calls, "import "+table)
	if f.archive == nil {
		f.archive = map[string][]string{}
	}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var tenant, day string
		fmt.Sscanf(l, "%s %s", &tenant, &day)
		k := table + "/" + tenant + "/" + day
		f.archive[k] = append(f.archive[k], l)
	}
	return nil
}
func (f *fakeSrc) Unload(_ context.Context, table, tenant string, day time.Time) error {
	f.calls = append(f.calls, "unload "+table)
	delete(f.archive, table+"/"+tenant+"/"+f.day(day))
	return nil
}
func (f *fakeSrc) LoadedDays(context.Context, string) ([]string, error) { return nil, nil }

func newSrc(now time.Time) *fakeSrc {
	d := func(n int) string { return now.AddDate(0, 0, -n).Format("2006-01-02") }
	row := func(t, day, msg string) string { return t + " " + day + " " + msg }
	return &fakeSrc{rows: map[string]map[string]map[string][]string{
		"otel_logs": {
			"acme":   {d(1): {row("acme", d(1), "a-log-1"), row("acme", d(1), "a-log-2")}, d(3): {row("acme", d(3), "old")}},
			"globex": {d(1): {row("globex", d(1), "g-secret")}},
			"bad/..": {d(1): {"x"}},
		},
		"otel_metrics": {"acme": {d(1): {row("acme", d(1), "m1")}}},
	}}
}

func gunzip(t *testing.T, path string) string {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(gz)
	return string(b)
}

func TestExportLayoutAndSkipping(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	src := newSrc(now)
	dir := t.TempDir()
	docs, _ := docstore.OpenFile(t.TempDir())
	docs.Create(context.Background(), "users", "bob", map[string]any{"name": "bob", "tenant": "acme"})
	docs.Create(context.Background(), "dashboards", "d1", map[string]any{"name": "Hosts", "tenant": "acme"})
	m := &Manager{Dir: dir, Src: src, Docs: docs, DataDays: 5, KeepDays: 0, Now: func() time.Time { return now }}
	if err := m.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "telemetry", "2026-09-30", "acme", "otel_logs.jsonl.gz")
	if got := gunzip(t, f); got != "acme 2026-09-30 a-log-1\nacme 2026-09-30 a-log-2\n" {
		t.Fatalf("file content: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "telemetry", "2026-09-30", "globex", "otel_logs.jsonl.gz")); err != nil {
		t.Fatal("each tenant gets its own file")
	}
	if _, err := os.Stat(filepath.Join(dir, "telemetry", "2026-09-30", "bad")); !os.IsNotExist(err) {
		t.Fatal("a tenant name that is not a safe directory name must be skipped")
	}
	for _, d := range []string{"2026-09-26", "2026-09-27", "2026-09-28", "2026-09-29", "2026-09-30"} { // 5 completed days, today is not exported
		if _, err := os.Stat(filepath.Join(dir, "telemetry", d, "DONE")); err != nil {
			t.Errorf("day %s must be marked complete (also days without any data, so they are not queried again)", d)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "telemetry", "2026-10-01")); !os.IsNotExist(err) {
		t.Fatal("today is still receiving data and must not be exported")
	}
	if _, err := os.Stat(filepath.Join(dir, "telemetry", ".tmp-2026-09-30")); !os.IsNotExist(err) {
		t.Fatal("no temporary directory may be left behind")
	}
	// the listing is per tenant and shows row counts
	l := m.List("acme")
	if len(l) != 2 || l[0].Day != "2026-09-30" || l[0].Rows["logs"] != 2 || l[0].Rows["metrics"] != 1 || l[0].Bytes == 0 || l[1].Day != "2026-09-28" || l[1].Rows["logs"] != 1 {
		// only days where this tenant has data are listed (days without any data are marked complete but not shown)
		t.Fatalf("list: %+v", l)
	}
	for _, d := range m.List("globex") {
		if d.Rows["metrics"] != 0 || d.Rows["logs"] > 1 {
			t.Fatalf("globex sees acme's rows: %+v", d)
		}
	}
	if len(m.List("nobody")) != 0 {
		t.Fatal("a tenant without data has no days")
	}
	// the second run exports nothing new, and does not even ask the database for old days
	src.calls = nil
	if err := m.RunOnce(context.Background()); err != nil || len(src.calls) != 0 {
		t.Fatalf("second run: %v %v", err, src.calls)
	}
	if st := m.State(); st.LastError != "" || st.LastRun.IsZero() || st.Running {
		t.Fatalf("state: %+v", st)
	}
	// config dump: for the operator, mode 0600, includes users and dashboards
	cfgs, _ := filepath.Glob(filepath.Join(dir, "config", "lumen-config-2026-10-01.json.gz"))
	if len(cfgs) != 1 {
		t.Fatal("expected the daily config dump")
	}
	if st, _ := os.Stat(cfgs[0]); st.Mode().Perm() != 0o600 {
		t.Fatalf("config dump must be private: %v", st.Mode())
	}
	if c := gunzip(t, cfgs[0]); !strings.Contains(c, `"bob"`) || !strings.Contains(c, `"Hosts"`) {
		t.Fatalf("config dump: %s", c)
	}
}

func TestFailureLeavesNoHalfDayAndRetries(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	src := newSrc(now)
	src.failOn = "otel_metrics"
	dir := t.TempDir()
	m := &Manager{Dir: dir, Src: src, DataDays: 3, Now: func() time.Time { return now }}
	if err := m.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "clickhouse is down") {
		t.Fatalf("the error must reach the caller: %v", err)
	}
	if m.State().LastError == "" {
		t.Fatal("the UI must be able to show the last error")
	}
	if _, err := os.Stat(filepath.Join(dir, "telemetry", "2026-09-30", "DONE")); !os.IsNotExist(err) {
		t.Fatal("a failed day must not be marked complete")
	}
	if _, err := os.Stat(filepath.Join(dir, "telemetry", ".tmp-2026-09-30")); !os.IsNotExist(err) {
		t.Fatal("a failed day must not leave partial files")
	}
	src.failOn = "" // the database is back: the next run completes the day
	if err := m.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.List("acme")) == 0 || m.State().LastError != "" {
		t.Fatal("retry must succeed and clear the error")
	}
}

func TestPrune(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	for _, d := range []string{"2026-01-01", "2026-09-20", "2026-09-30"} {
		os.MkdirAll(filepath.Join(dir, "telemetry", d, "acme"), 0o750)
		os.WriteFile(filepath.Join(dir, "telemetry", d, "DONE"), []byte(`{"tenants":{"acme":{}}}`), 0o640)
	}
	os.MkdirAll(filepath.Join(dir, "telemetry", "not-a-day"), 0o750)
	m := &Manager{Dir: dir, Src: newSrc(now), DataDays: 0, KeepDays: 30, Now: func() time.Time { return now }}
	if err := m.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "telemetry", "2026-01-01")); !os.IsNotExist(err) {
		t.Fatal("a day older than the backup retention must be removed")
	}
	for _, d := range []string{"2026-09-20", "2026-09-30", "not-a-day"} {
		if _, err := os.Stat(filepath.Join(dir, "telemetry", d)); err != nil {
			t.Fatalf("%s must be kept", d)
		}
	}
	m.KeepDays = 0 // keep forever
	os.MkdirAll(filepath.Join(dir, "telemetry", "2020-01-01", "acme"), 0o750)
	m.RunOnce(context.Background())
	if _, err := os.Stat(filepath.Join(dir, "telemetry", "2020-01-01")); err != nil {
		t.Fatal("retention 0 keeps everything")
	}
}

func TestLoadUnloadAndDownloadAreTenantScoped(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	src := newSrc(now)
	dir := t.TempDir()
	m := &Manager{Dir: dir, Src: src, DataDays: 3, Now: func() time.Time { return now }}
	if err := m.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := m.Load(ctx, "acme", "2026-09-30"); err != nil {
		t.Fatal(err)
	}
	if n := len(src.archive["otel_logs/acme/2026-09-30"]); n != 2 {
		t.Fatalf("both log rows must be in the archive, got %d", n)
	}
	if len(src.archive["otel_logs/globex/2026-09-30"]) != 0 {
		t.Fatal("loading acme's day must not bring in globex's rows")
	}
	// loading twice must not duplicate: unload comes first
	src.calls = nil
	if err := m.Load(ctx, "acme", "2026-09-30"); err != nil {
		t.Fatal(err)
	}
	if n := len(src.archive["otel_logs/acme/2026-09-30"]); n != 2 {
		t.Fatalf("loading twice must be idempotent, got %d rows", n)
	}
	if !strings.HasPrefix(strings.Join(src.calls, ","), "unload otel_spans,unload otel_logs,unload otel_metrics,import") {
		t.Fatalf("unload must come before import: %v", src.calls)
	}
	if err := m.Unload(ctx, "acme", "2026-09-30"); err != nil || len(src.archive["otel_logs/acme/2026-09-30"]) != 0 {
		t.Fatalf("unload: %v", err)
	}
	if err := m.Load(ctx, "nobody", "2026-09-30"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a tenant without a backup of that day: %v", err)
	}
	if err := m.Load(ctx, "acme", "2026-08-01"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a day without a backup: %v", err)
	}
	// strict validation: no path tricks via day, tenant or table
	for _, d := range []string{"../../etc", "2026-09-30/../..", "20260930", "2026-13-45", "", "2026-09-30 "} {
		if err := m.Load(ctx, "acme", d); !errors.Is(err, ErrInvalid) {
			t.Errorf("day %q must be invalid, got %v", d, err)
		}
		if _, err := m.File("acme", d, "logs"); !errors.Is(err, ErrInvalid) {
			t.Errorf("File day %q: %v", d, err)
		}
	}
	for _, tn := range []string{"..", "../globex", "acme/../globex", "", "a b"} {
		if _, err := m.File(tn, "2026-09-30", "logs"); !errors.Is(err, ErrNotFound) {
			t.Errorf("tenant %q must not resolve: %v", tn, err)
		}
	}
	for _, tb := range []string{"../logs", "otel_logs", "x", ""} {
		if _, err := m.File("acme", "2026-09-30", tb); !errors.Is(err, ErrInvalid) {
			t.Errorf("table %q: %v", tb, err)
		}
	}
	p, err := m.File("acme", "2026-09-30", "logs")
	if err != nil || !strings.HasPrefix(p, filepath.Join(dir, "telemetry")) || strings.Contains(gunzip(t, p), "globex") {
		t.Fatalf("download path %q err %v", p, err)
	}
	if _, err := m.File("acme", "2026-09-30", "spans"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a table with no rows that day has no file: %v", err)
	}
}

func TestOnlyOneOperationAtATime(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	m := &Manager{Dir: t.TempDir(), Src: newSrc(now), DataDays: 1, Now: func() time.Time { return now }}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.RunOnce(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if err := m.Unload(context.Background(), "acme", "2026-09-30"); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	var b bytes.Buffer
	_ = b
}
