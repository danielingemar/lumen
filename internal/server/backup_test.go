package server

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/backup"
	"github.com/danielingemar/lumen/internal/dashboards"
	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/store"
)

type bkSrc struct{ loaded map[string]map[string]bool }

func (b *bkSrc) Tenants(_ context.Context, table string, day time.Time) ([]string, error) {
	if table == "otel_logs" {
		return []string{"acme", "globex"}, nil
	}
	return nil, nil
}
func (b *bkSrc) Export(_ context.Context, _, tenant string, _ time.Time, w io.Writer) error {
	fmt.Fprintf(w, "{\"tenant\":%q,\"body\":\"hello\"}\n", tenant)
	return nil
}
func (b *bkSrc) Import(_ context.Context, _ string, r io.Reader) error {
	data, _ := io.ReadAll(r)
	var x struct{ Tenant string }
	json.Unmarshal(data, &x)
	if b.loaded[x.Tenant] == nil {
		b.loaded[x.Tenant] = map[string]bool{}
	}
	b.loaded[x.Tenant]["2026-09-30"] = true
	return nil
}
func (b *bkSrc) Unload(_ context.Context, _, tenant string, _ time.Time) error {
	delete(b.loaded[tenant], "2026-09-30")
	return nil
}
func (b *bkSrc) LoadedDays(_ context.Context, tenant string) ([]string, error) {
	var out []string
	for d := range b.loaded[tenant] {
		out = append(out, d)
	}
	return out, nil
}

func TestBackupAPI(t *testing.T) {
	f, _ := docstore.OpenFile(t.TempDir())
	st, _ := auth.Open(f, 0)
	st.CreateUser("admin", "acme", "admins-long-password")
	st.CreateUserIn("reader", "acme", "readers-long-password", "user")
	st.CreateUser("globexadmin", "globex", "globex-long-password")
	g, _ := st.CreateGroup("acme", "Auditor", "", map[string]string{"backups": "read", "logs": "read"})
	st.CreateUserIn("auditor", "acme", "auditor-long-password", g.ID)
	a := auth.New(st, nil, false)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	src := &bkSrc{loaded: map[string]map[string]bool{}}
	m := &backup.Manager{Dir: t.TempDir(), Src: src, DataDays: 1, Now: func() time.Time { return now }}
	if err := m.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	fs := &fakeStore{}
	ts := httptest.NewServer(New(fs, a, slog.New(slog.NewTextHandler(io.Discard, nil))).WithAuth(a).WithDashboards(dashboards.New(f)).
		WithBackups(m, BackupInfo{DataDays: 30, KeepDays: 365}).Handler())
	defer ts.Close()
	ad, rd, au, gx := client(), client(), client(), client()
	login(t, ad, ts, "admin", "admins-long-password")
	login(t, rd, ts, "reader", "readers-long-password")
	login(t, au, ts, "auditor", "auditor-long-password")
	login(t, gx, ts, "globexadmin", "globex-long-password")

	if c := code(rd, "GET", ts.URL+"/api/v1/backups", ""); c != 403 {
		t.Fatalf("a plain user has no access to backups: %d", c)
	}
	var list struct {
		Enabled bool
		Days    []struct {
			Day    string
			Loaded bool
			Rows   map[string]int64
		}
		Info BackupInfo
	}
	_, b := do(ad, "GET", ts.URL+"/api/v1/backups", "")
	json.Unmarshal(b, &list)
	if !list.Enabled || len(list.Days) != 1 || list.Days[0].Day != "2026-09-30" || list.Days[0].Rows["logs"] != 1 || list.Days[0].Loaded || list.Info.DataDays != 30 {
		t.Fatalf("list: %s", b)
	}
	// read access (auditor) may list and download but not load or run
	if c := code(au, "GET", ts.URL+"/api/v1/backups", ""); c != 200 {
		t.Fatalf("auditor list: %d", c)
	}
	if code(au, "POST", ts.URL+"/api/v1/backups/2026-09-30/load", "") != 403 || code(au, "POST", ts.URL+"/api/v1/backups/run", "") != 403 || code(au, "DELETE", ts.URL+"/api/v1/backups/2026-09-30/load", "") != 403 {
		t.Fatal("read access must not allow load, unload or run")
	}
	resp, err := au.Get(ts.URL + "/api/v1/backups/2026-09-30/logs")
	if err != nil || resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/gzip" || !strings.Contains(resp.Header.Get("Content-Disposition"), "acme-2026-09-30-logs") {
		t.Fatalf("download: %v %v", err, resp)
	}
	gz, _ := gzip.NewReader(resp.Body)
	body, _ := io.ReadAll(gz)
	if !strings.Contains(string(body), `"acme"`) || strings.Contains(string(body), "globex") {
		t.Fatalf("a tenant must only get its own rows: %s", body)
	}
	// load, see it flagged, unload
	if c := code(ad, "POST", ts.URL+"/api/v1/backups/2026-09-30/load", ""); c != 200 {
		t.Fatalf("load: %d", c)
	}
	_, b = do(ad, "GET", ts.URL+"/api/v1/backups", "")
	json.Unmarshal(b, &list)
	if !list.Days[0].Loaded {
		t.Fatalf("loaded flag: %s", b)
	}
	// bad input and other tenants
	for _, p := range []string{"/api/v1/backups/2026-9-30/load", "/api/v1/backups/..%2F..%2Fetc/load", "/api/v1/backups/2026-02-30/load"} {
		if c := code(ad, "POST", ts.URL+p, ""); c != 400 && c != 404 {
			t.Errorf("%s must be refused, got %d", p, c)
		}
	}
	if c := code(ad, "GET", ts.URL+"/api/v1/backups/2026-09-30/metrics", ""); c != 404 {
		t.Fatalf("a table without rows has no file: %d", c)
	}
	if c := code(ad, "GET", ts.URL+"/api/v1/backups/2026-09-30/etc", ""); c != 400 {
		t.Fatalf("unknown table: %d", c)
	}
	if c := code(ad, "POST", ts.URL+"/api/v1/backups/2026-01-01/load", ""); c != 404 {
		t.Fatalf("a day without backup: %d", c)
	}
	_, b = do(gx, "GET", ts.URL+"/api/v1/backups", "")
	if strings.Contains(string(b), "acme") || !strings.Contains(string(b), "2026-09-30") {
		t.Fatalf("globex sees only its own day: %s", b)
	}
	if c := code(ad, "DELETE", ts.URL+"/api/v1/backups/2026-09-30/load", ""); c != 200 || len(src.loaded["acme"]) != 0 || len(src.loaded["globex"]) != 0 {
		t.Fatalf("unload: %d %+v", c, src.loaded)
	}
	// archive switch on reads: needs backups:read, and switches the data source
	if c := code(rd, "GET", ts.URL+"/api/v1/logs?archive=1", ""); c != 403 {
		t.Fatalf("a plain user must not read the archive: %d", c)
	}
	if c := code(au, "GET", ts.URL+"/api/v1/logs", ""); c != 200 || fs.archive {
		t.Fatalf("normal read must use live data: %d %v", c, fs.archive)
	}
	if c := code(au, "GET", ts.URL+"/api/v1/logs?archive=1", ""); c != 200 || !fs.archive {
		t.Fatalf("an auditor reads the archive: %d archive=%v", c, fs.archive)
	}
	// the archive flag must not leak into the next request
	if c := code(au, "GET", ts.URL+"/api/v1/logs", ""); c != 200 || fs.archive {
		t.Fatal("archive flag leaked")
	}
	_ = store.InArchive
	_ = http.StatusOK
}
