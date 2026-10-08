package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/backup"
	"github.com/danielingemar/lumen/internal/dashboards"
	"github.com/danielingemar/lumen/internal/docstore"
)

func TestWhereBackupsAreKeptIsChosenInSettings(t *testing.T) {
	f, _ := docstore.OpenFile(t.TempDir())
	st, _ := auth.Open(f, 0)
	st.CreateUser("admin", "acme", "admins-long-password")
	st.CreateUserIn("reader", "acme", "readers-long-password", "user")
	st.CreateUser("globexadmin", "globex", "globex-long-password")
	a := auth.New(st, nil, false)
	root := t.TempDir()
	def := filepath.Join(root, "default")
	disk := filepath.Join(root, "disk")
	os.Mkdir(def, 0o755)
	os.Mkdir(disk, 0o755)
	os.MkdirAll(filepath.Join(def, "telemetry", "2026-09-30"), 0o755)
	os.MkdirAll(filepath.Join(def, "telemetry", "2026-10-01"), 0o755)
	where := backup.NewWhere(def, false)
	srv := New(&fakeStore{}, a, slog.New(slog.NewTextHandler(io.Discard, nil))).WithAuth(a).WithDashboards(dashboards.New(f)).WithOwnerTenant("acme").
		WithBackupLocation(&BackupWhere{DB: f, Where: where, Default: def, Roots: []string{root}, DataDir: t.TempDir()})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ad, rd, gx := client(), client(), client()
	login(t, ad, ts, "admin", "admins-long-password")
	login(t, rd, ts, "reader", "readers-long-password")
	login(t, gx, ts, "globexadmin", "globex-long-password")
	get := func() (string, string, int, map[string]any) {
		_, b := do(ad, "GET", ts.URL+"/api/v1/backup-location", "")
		var o struct {
			Dir    string
			Source string
			Days   int
			Info   map[string]any
		}
		json.Unmarshal(b, &o)
		return o.Dir, o.Source, o.Days, o.Info
	}
	if dir, src, days, _ := get(); dir != def || src != "default" || days != 2 {
		t.Fatalf("to begin with: the default folder, with its two days of backups: %q %q %d", dir, src, days)
	}
	// who may
	if c := code(rd, "GET", ts.URL+"/api/v1/backup-location", ""); c != 403 {
		t.Fatalf("without the backups permission: %d", c)
	}
	if r, b := do(gx, "GET", ts.URL+"/api/v1/backup-location", ""); r.StatusCode != 403 || !strings.Contains(string(b), "the tenant that runs the installation") {
		t.Fatalf("another tenant: %d %s", r.StatusCode, b)
	}
	if c := code(gx, "PUT", ts.URL+"/api/v1/backup-location", `{"dir":"`+disk+`"}`); c != 403 {
		t.Fatalf("and cannot change it: %d", c)
	}
	// looking at a folder first
	r, b := do(ad, "POST", ts.URL+"/api/v1/backup-location/check", `{"dir":"`+disk+`"}`)
	if r.StatusCode != 200 || !strings.Contains(string(b), `"writable":true`) || !strings.Contains(string(b), `"exists":true`) {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	if _, b = do(ad, "POST", ts.URL+"/api/v1/backup-location/check", `{"dir":"`+filepath.Join(root, "nodisk")+`"}`); !strings.Contains(string(b), "does not exist here") {
		t.Fatalf("a disk that is not mounted: %s", b)
	}
	if where.Get() != def {
		t.Fatal("checking changes nothing")
	}
	// choosing is refused for what cannot be used, and nothing changes
	for name, dir := range map[string]string{"missing": filepath.Join(root, "nodisk"), "outside": "/etc", "relative": "mnt/backup", "dots": root + "/disk/../disk"} {
		if r, b := do(ad, "PUT", ts.URL+"/api/v1/backup-location", `{"dir":"`+dir+`"}`); r.StatusCode != 400 || where.Get() != def {
			t.Errorf("%s: %d %s", name, r.StatusCode, b)
		}
	}
	// choosing a folder that is fine: used from now on, and remembered
	r, b = do(ad, "PUT", ts.URL+"/api/v1/backup-location", `{"dir":"`+disk+`"}`)
	if r.StatusCode != 200 || !strings.Contains(string(b), `"previous_days":2`) || where.Get() != disk || !where.Strict() {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	if backup.LoadSaved(context.Background(), f) != disk {
		t.Fatal("remembered, so that it is still used after a restart")
	}
	if dir, src, days, _ := get(); dir != disk || src != "chosen" || days != 0 {
		t.Fatalf("%q %q %d", dir, src, days)
	}
	// back to the default
	if r, _ := do(ad, "PUT", ts.URL+"/api/v1/backup-location", `{"dir":""}`); r.StatusCode != 200 || where.Get() != def || where.Strict() || backup.LoadSaved(context.Background(), f) != "" {
		t.Fatalf("%d %q", r.StatusCode, where.Get())
	}
}
