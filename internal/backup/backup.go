// Package backup keeps a copy of telemetry before ClickHouse expires it, and brings old days back for viewing.
//
// Every completed UTC day is exported once, per tenant and table, as gzip-compressed JSON lines:
//
//	<dir>/telemetry/YYYY-MM-DD/<tenant>/otel_logs.jsonl.gz   (also otel_spans, otel_metrics)
//	<dir>/telemetry/YYYY-MM-DD/DONE                           (written last: the day is complete)
//	<dir>/config/lumen-config-YYYY-MM-DD.json.gz              (users, groups, dashboards, hosts, instances)
//
// "Loading" a day copies it into the *_archive tables, where the UI can query it (Archive switch). The files are plain
// gzip, so they can also be read with zcat or loaded into any other tool.
package backup

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
)

// Tables are the telemetry tables that are backed up, with the short names used in the API.
var Tables = []string{"otel_spans", "otel_logs", "otel_metrics"}

var short = map[string]string{"spans": "otel_spans", "logs": "otel_logs", "metrics": "otel_metrics"}

var (
	ErrBusy     = errors.New("a backup or restore is already running; try again in a few minutes")
	ErrNotFound = errors.New("no such backup")
	ErrInvalid  = errors.New("invalid request")

	dayRe    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	tenantRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

// Source is the telemetry database (implemented by store.ClickHouse).
type Source interface {
	Tenants(ctx context.Context, table string, day time.Time) ([]string, error)
	Export(ctx context.Context, table, tenant string, day time.Time, w io.Writer) error
	Import(ctx context.Context, table string, r io.Reader) error
	Unload(ctx context.Context, table, tenant string, day time.Time) error
	LoadedDays(ctx context.Context, tenant string) ([]string, error)
}

type Manager struct {
	Dir      string
	Src      Source
	Docs     docstore.Backend // for the daily configuration dump; may be nil
	DataDays int              // days of live data in ClickHouse: how far back a first run backfills
	KeepDays int              // how long backups are kept (0 = forever)
	Log      *slog.Logger
	Now      func() time.Time

	mu sync.Mutex
	st State
	sm sync.Mutex // guards st
}

// State is what the UI shows about the job.
type State struct {
	Running   bool      `json:"running"`
	LastRun   time.Time `json:"last_run"`
	LastError string    `json:"last_error"`
	LastDays  int       `json:"last_days_exported"`
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *Manager) logf() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.Default()
}

func (m *Manager) State() State            { m.sm.Lock(); defer m.sm.Unlock(); return m.st }
func (m *Manager) setState(f func(*State)) { m.sm.Lock(); f(&m.st); m.sm.Unlock() }

func (m *Manager) telemetryDir() string { return filepath.Join(m.Dir, "telemetry") }

type fileInfo struct {
	Rows  int64 `json:"rows"`
	Bytes int64 `json:"bytes"`
}

type doneMeta struct {
	ExportedAt time.Time                      `json:"exported_at"`
	Tenants    map[string]map[string]fileInfo `json:"tenants"` // tenant -> table -> info
}

type countWriter struct {
	w     io.Writer
	lines int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if b == '\n' {
			c.lines++
		}
	}
	return c.w.Write(p)
}

func (m *Manager) exportDay(ctx context.Context, day time.Time) error {
	d := day.Format("2006-01-02")
	root := m.telemetryDir()
	tmp, final := filepath.Join(root, ".tmp-"+d), filepath.Join(root, d)
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		return err
	}
	meta := doneMeta{Tenants: map[string]map[string]fileInfo{}}
	for _, table := range Tables {
		tenants, err := m.Src.Tenants(ctx, table, day)
		if err != nil {
			_ = os.RemoveAll(tmp)
			return fmt.Errorf("%s %s: %w", d, table, err)
		}
		for _, tenant := range tenants {
			if !tenantRe.MatchString(tenant) {
				m.logf().Warn("backup: skipping a tenant with an unsupported name", "tenant", tenant)
				continue
			}
			if err := os.MkdirAll(filepath.Join(tmp, tenant), 0o750); err != nil {
				return err
			}
			path := filepath.Join(tmp, tenant, table+".jsonl.gz")
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
			if err != nil {
				return err
			}
			gz := gzip.NewWriter(f)
			cw := &countWriter{w: gz}
			err = m.Src.Export(ctx, table, tenant, day, cw)
			if cerr := gz.Close(); err == nil {
				err = cerr
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				_ = os.RemoveAll(tmp)
				return fmt.Errorf("%s %s %s: %w", d, tenant, table, err)
			}
			st, _ := os.Stat(path)
			if meta.Tenants[tenant] == nil {
				meta.Tenants[tenant] = map[string]fileInfo{}
			}
			meta.Tenants[tenant][table] = fileInfo{Rows: cw.lines, Bytes: st.Size()}
		}
	}
	meta.ExportedAt = m.now()
	b, _ := json.MarshalIndent(meta, "", " ")
	if err := os.WriteFile(filepath.Join(tmp, "DONE"), b, 0o640); err != nil {
		return err
	}
	_ = os.RemoveAll(final)
	return os.Rename(tmp, final) // the day appears all at once, or not at all
}

// RunOnce exports every completed day that has no backup yet, prunes old backups and writes the daily config dump.
func (m *Manager) RunOnce(ctx context.Context) error {
	if !m.mu.TryLock() {
		return ErrBusy
	}
	defer m.mu.Unlock()
	m.setState(func(s *State) { s.Running = true })
	n, err := m.runLocked(ctx)
	m.setState(func(s *State) {
		s.Running, s.LastRun, s.LastDays = false, m.now(), n
		s.LastError = ""
		if err != nil {
			s.LastError = err.Error()
		}
	})
	return err
}

func (m *Manager) runLocked(ctx context.Context) (int, error) {
	if err := os.MkdirAll(m.telemetryDir(), 0o750); err != nil {
		return 0, fmt.Errorf("the backup directory %s is not writable: %w", m.Dir, err)
	}
	today := m.now().Truncate(24 * time.Hour)
	exported := 0
	for i := m.DataDays; i >= 1; i-- {
		day := today.AddDate(0, 0, -i)
		if _, err := os.Stat(filepath.Join(m.telemetryDir(), day.Format("2006-01-02"), "DONE")); err == nil {
			continue
		}
		if err := m.exportDay(ctx, day); err != nil {
			return exported, err
		}
		exported++
		m.logf().Info("backup: exported day", "day", day.Format("2006-01-02"))
	}
	m.prune(today)
	if err := m.dumpConfig(ctx, today); err != nil {
		return exported, fmt.Errorf("configuration dump: %w", err)
	}
	return exported, nil
}

func (m *Manager) prune(today time.Time) {
	if m.KeepDays <= 0 {
		return
	}
	cutoff := today.AddDate(0, 0, -m.KeepDays).Format("2006-01-02")
	entries, _ := os.ReadDir(m.telemetryDir())
	for _, e := range entries {
		if e.IsDir() && dayRe.MatchString(e.Name()) && e.Name() < cutoff {
			if os.RemoveAll(filepath.Join(m.telemetryDir(), e.Name())) == nil {
				m.logf().Info("backup: removed day older than the backup retention", "day", e.Name())
			}
		}
	}
}

var configColls = []string{"users", "keys", "groups", "dashboards", "hosts", "instances", "meta", "alert_rules", "alert_channels", "alert_silences"}

// dumpConfig writes users (password hashes), groups, dashboards, hosts and instances (credentials stay encrypted)
// for ALL tenants. It is for the operator: it is never served through the API. The last 30 are kept.
func (m *Manager) dumpConfig(ctx context.Context, today time.Time) error {
	if m.Docs == nil {
		return nil
	}
	dir := filepath.Join(m.Dir, "config")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name := filepath.Join(dir, "lumen-config-"+today.Format("2006-01-02")+".json.gz")
	if _, err := os.Stat(name); err == nil {
		return nil
	}
	all := map[string][]json.RawMessage{}
	for _, c := range configColls {
		docs, err := m.Docs.List(ctx, c, nil, 10000)
		if err != nil {
			return err
		}
		for _, d := range docs {
			all[c] = append(all[c], d.Data)
		}
	}
	tmp := name + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	err = json.NewEncoder(gz).Encode(all)
	if cerr := gz.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, name); err != nil {
		return err
	}
	files, _ := filepath.Glob(filepath.Join(dir, "lumen-config-*.json.gz"))
	sort.Strings(files)
	for len(files) > 30 {
		os.Remove(files[0])
		files = files[1:]
	}
	return nil
}

// Run backs up in the background: first shortly after start, then every hour.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := m.RunOnce(ctx); err != nil && !errors.Is(err, ErrBusy) {
			m.logf().Error("backup failed", "err", err)
		}
		t.Reset(time.Hour)
	}
}

// Loaded lists the days of a tenant that are currently in the archive tables.
func (m *Manager) Loaded(ctx context.Context, tenant string) ([]string, error) {
	return m.Src.LoadedDays(ctx, tenant)
}

// Day is one backed-up day as the UI shows it.
type Day struct {
	Day   string           `json:"day"`
	Rows  map[string]int64 `json:"rows"` // spans, logs, metrics
	Bytes int64            `json:"bytes"`
}

func shortName(table string) string { return strings.TrimPrefix(table, "otel_") }

// List returns the days that have a backup for this tenant, newest first.
func (m *Manager) List(tenant string) []Day {
	out := []Day{}
	entries, _ := os.ReadDir(m.telemetryDir())
	for _, e := range entries {
		if !e.IsDir() || !dayRe.MatchString(e.Name()) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(m.telemetryDir(), e.Name(), "DONE"))
		if err != nil {
			continue
		}
		var meta doneMeta
		if json.Unmarshal(raw, &meta) != nil {
			continue
		}
		files, ok := meta.Tenants[tenant]
		if !ok {
			continue
		}
		d := Day{Day: e.Name(), Rows: map[string]int64{}}
		for table, fi := range files {
			d.Rows[shortName(table)] = fi.Rows
			d.Bytes += fi.Bytes
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day > out[j].Day })
	return out
}

func parseDay(day string) (time.Time, error) {
	if !dayRe.MatchString(day) {
		return time.Time{}, fmt.Errorf("%w: the day must look like 2026-09-30", ErrInvalid)
	}
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: not a date", ErrInvalid)
	}
	return t, nil
}

func (m *Manager) tenantDir(tenant, day string) (string, error) {
	if !tenantRe.MatchString(tenant) {
		return "", ErrNotFound
	}
	if _, err := os.Stat(filepath.Join(m.telemetryDir(), day, "DONE")); err != nil {
		return "", ErrNotFound
	}
	dir := filepath.Join(m.telemetryDir(), day, tenant)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", ErrNotFound
	}
	return dir, nil
}

// File returns the path of one backup file for the download. table is spans, logs or metrics.
func (m *Manager) File(tenant, day, table string) (string, error) {
	if _, err := parseDay(day); err != nil {
		return "", err
	}
	full, ok := short[table]
	if !ok {
		return "", fmt.Errorf("%w: table must be spans, logs or metrics", ErrInvalid)
	}
	dir, err := m.tenantDir(tenant, day)
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, full+".jsonl.gz")
	if _, err := os.Stat(p); err != nil {
		return "", ErrNotFound
	}
	return p, nil
}

// Unload removes a day from the archive tables (the tenant's rows only).
func (m *Manager) Unload(ctx context.Context, tenant, day string) error {
	d, err := parseDay(day)
	if err != nil {
		return err
	}
	if !m.mu.TryLock() {
		return ErrBusy
	}
	defer m.mu.Unlock()
	for _, t := range Tables {
		if err := m.Src.Unload(ctx, t, tenant, d); err != nil {
			return err
		}
	}
	return nil
}

// Load copies a backed-up day into the archive tables. It first removes that day from the archive, so loading twice
// never duplicates rows.
func (m *Manager) Load(ctx context.Context, tenant, day string) error {
	d, err := parseDay(day)
	if err != nil {
		return err
	}
	dir, err := m.tenantDir(tenant, day)
	if err != nil {
		return err
	}
	if !m.mu.TryLock() {
		return ErrBusy
	}
	defer m.mu.Unlock()
	for _, t := range Tables {
		if err := m.Src.Unload(ctx, t, tenant, d); err != nil {
			return err
		}
	}
	for _, t := range Tables {
		f, err := os.Open(filepath.Join(dir, t+".jsonl.gz"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return fmt.Errorf("%s is damaged: %w", t, err)
		}
		err = m.Src.Import(ctx, t, gz)
		gz.Close()
		f.Close()
		if err != nil {
			return fmt.Errorf("loading %s: %w", t, err)
		}
	}
	return nil
}
