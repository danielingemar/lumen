package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/alerts"
	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/dashboards"
	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/health"
	"github.com/danielingemar/lumen/internal/secretbox"
)

type chFake struct{ ch health.CH }

func (c chFake) Health(context.Context) (health.CH, error) { return c.ch, nil }

func TestLumensOwnHealth(t *testing.T) {
	f, _ := docstore.OpenFile(t.TempDir())
	st, _ := auth.Open(f, 0)
	st.CreateUser("admin", "acme", "admins-long-password")
	st.CreateUserIn("reader", "acme", "readers-long-password", "user")
	st.CreateUser("globexadmin", "globex", "globex-long-password")
	a := auth.New(st, nil, false)
	box, _ := secretbox.New("k")
	const gb = 1 << 30
	free := uint64(6 * gb) // 94 percent used
	m := health.New(health.Sources{Paths: map[string]string{"data": "/data"}, CH: chFake{health.CH{Disks: []health.Disk{{Name: "default", Total: 100 * gb, Free: 50 * gb}}, Tables: []health.Table{{Name: "default.otel_logs", Bytes: 9 * gb}}}}}, 80, 90)
	m.Statfs = func(string) (uint64, uint64, error) { return 100 * gb, free, nil }
	eng := &alerts.Engine{DB: f, Box: box, Eval: &alerts.Evaluator{Q: &alertQ{vals: map[string]float64{}}, St: noStatus{}}, Guard: alerts.Guard{AllowPrivate: true}, GroupWait: time.Second}
	srv := New(&fakeStore{}, a, slog.New(slog.NewTextHandler(io.Discard, nil))).WithAuth(a).WithDashboards(dashboards.New(f)).WithAlerts(eng).WithHealth(m).WithOwnerTenant("acme")
	eng.Eval.H = srv
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ad, rd, gx := client(), client(), client()
	login(t, ad, ts, "admin", "admins-long-password")
	login(t, rd, ts, "reader", "readers-long-password")
	login(t, gx, ts, "globexadmin", "globex-long-password")
	// the report: what is wrong, how much is left, what is big
	r, b := do(ad, "GET", ts.URL+"/api/v1/health", "")
	var out struct {
		Report health.Report
		Warn   float64
		Crit   float64
	}
	json.Unmarshal(b, &out)
	if r.StatusCode != 200 || out.Report.Overall != "critical" || len(out.Report.Problems) != 1 || out.Warn != 80 || out.Crit != 90 || len(out.Report.Disks) != 1 || out.Report.CH == nil || out.Report.CH.Tables[0].Name != "default.otel_logs" {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	if p := out.Report.Problems[0]; p.Level != "critical" || !strings.Contains(p.Text, "94% full") || !strings.Contains(p.Text, "/data") {
		t.Fatalf("%+v", p)
	}
	// it is kept for a while, and refresh=1 looks again
	free = 60 * gb
	_, b = do(ad, "GET", ts.URL+"/api/v1/health", "")
	if !strings.Contains(string(b), `"overall":"critical"`) {
		t.Fatal("kept")
	}
	_, b = do(ad, "GET", ts.URL+"/api/v1/health?refresh=1", "")
	if !strings.Contains(string(b), `"overall":"ok"`) {
		t.Fatalf("refresh: %s", b)
	}
	// who may see it
	if c := code(rd, "GET", ts.URL+"/api/v1/health", ""); c != 403 {
		t.Fatalf("a user without the settings permission: %d", c)
	}
	if r, b := do(gx, "GET", ts.URL+"/api/v1/health", ""); r.StatusCode != 403 || !strings.Contains(string(b), "the tenant that runs it") {
		t.Fatalf("another customer of the installation never sees it: %d %s", r.StatusCode, b)
	}
	// the page is told only when it may show it
	if _, b := do(ad, "GET", ts.URL+"/api/v1/me", ""); !strings.Contains(string(b), `"health":true`) {
		t.Fatalf("%s", b)
	}
	if _, b := do(gx, "GET", ts.URL+"/api/v1/me", ""); strings.Contains(string(b), `"health"`) {
		t.Fatalf("%s", b)
	}
	// the rules about Lumen itself: offered to the owner and made by the owner only
	_, b = do(ad, "GET", ts.URL+"/api/v1/alerts/templates", "")
	if !strings.Contains(string(b), "Lumen: a disk is more than 80% full") {
		t.Fatalf("the owner is offered the rules about Lumen itself: %s", b)
	}
	_, b = do(gx, "GET", ts.URL+"/api/v1/alerts/templates", "")
	if strings.Contains(string(b), "Lumen:") || !strings.Contains(string(b), "Nextcloud") {
		t.Fatalf("another tenant is not, but gets the others: %s", b)
	}
	rule := `{"name":"Disk","kind":"status","status":"lumen_disk","threshold":85}`
	if c := code(gx, "POST", ts.URL+"/api/v1/alerts/rules", rule); c != 403 {
		t.Fatalf("another tenant cannot make one: %d", c)
	}
	if r, b := do(ad, "POST", ts.URL+"/api/v1/alerts/rules", rule); r.StatusCode != 200 || !strings.Contains(string(b), `"threshold":85`) {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	// the source the alert engine reads
	if rep, _ := srv.Health(context.Background(), "acme"); len(rep.Disks) != 1 {
		t.Fatal("the owner's rules see the report")
	}
	if rep, _ := srv.Health(context.Background(), "globex"); len(rep.Disks) != 0 {
		t.Fatal("nobody else's do")
	}
}
