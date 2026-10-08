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

	"github.com/danielingemar/lumen/internal/audit"
	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/billing"
	"github.com/danielingemar/lumen/internal/dashboards"
	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/registry"
	"github.com/danielingemar/lumen/internal/secretbox"
)

type usageSrc struct{ by map[string][]billing.Sample }

func (u usageSrc) InstanceUsage(ctx context.Context, tenant string, from, to time.Time) ([]billing.Sample, error) {
	return u.by[tenant], nil
}

func fp(v float64) *float64 { return &v }

func TestBillingPricesAndInvoiceBasis(t *testing.T) {
	f, _ := docstore.OpenFile(t.TempDir())
	st, _ := auth.Open(f, 0)
	st.CreateUser("admin", "acme", "admins-long-password")
	st.CreateUserIn("reader", "acme", "readers-long-password", "user")
	st.CreateUser("globexadmin", "globex", "globex-long-password")
	a := auth.New(st, nil, false)
	box, _ := secretbox.New("k")
	reg := registry.New(f, box)
	if _, err := reg.CreateInstance("acme", registry.InstanceIn{Name: "kund-ab", URL: "https://kund.example.com", Host: "h1", Username: "monitor", Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	var s []billing.Sample
	for d := 1; d <= 31; d++ {
		s = append(s, billing.Sample{Instance: "kund.example.com", Day: time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC).Format("2006-01-02"),
			DayUsage: billing.DayUsage{Users: fp(12), Bytes: fp(33.3333e9)}})
	}
	rec := &billing.Recorder{DB: f, Src: usageSrc{map[string][]billing.Sample{"acme": s}}, Tenants: func() []string { return []string{"acme"} }, Now: func() time.Time { return now }}
	al := audit.New(f)
	srv := New(&fakeStore{}, a, slog.New(slog.NewTextHandler(io.Discard, nil))).WithAuth(a).WithDashboards(dashboards.New(f)).WithRegistry(reg).WithAudit(al).
		WithBilling(&Billing{Svc: billing.New(f), DB: f, Rec: rec, Now: func() time.Time { return now }})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ad, rd, gx := client(), client(), client()
	login(t, ad, ts, "admin", "admins-long-password")
	login(t, rd, ts, "reader", "readers-long-password")
	login(t, gx, ts, "globexadmin", "globex-long-password")
	u := ts.URL + "/api/v1/billing"

	// who may: the prices say what each customer pays
	if c := code(rd, "GET", u, ""); c != 403 {
		t.Fatalf("a reader: %d", c)
	}
	if c := code(rd, "GET", u+"/report", ""); c != 403 {
		t.Fatalf("a reader: %d", c)
	}
	if c := code(rd, "PUT", u, `{}`); c != 403 {
		t.Fatalf("a reader: %d", c)
	}
	// to begin with nothing is charged, and the choices are on offer
	_, b := do(ad, "GET", u, "")
	var g struct {
		Config     billing.Config
		Currencies []billing.Currency
		Instances  []map[string]any
		Periods    []string
	}
	json.Unmarshal(b, &g)
	if g.Config.UserPrice != "0.00" || len(g.Currencies) < 8 || len(g.Periods) != 12 || g.Periods[0] != "2026-11" || len(g.Instances) != 1 || g.Instances[0]["has_login"] != true {
		t.Fatalf("%s", b)
	}
	// refused input changes nothing
	for _, bad := range []string{`{"currency":"XXX"}`, `{"user_price":"-5"}`, `{"gb_price":"cheap"}`, `{"vat_percent":"250"}`, `{"profiles":[{"key":"bad key"}]}`} {
		if r, _ := do(ad, "PUT", u, bad); r.StatusCode != 400 {
			t.Fatalf("%s: %d", bad, r.StatusCode)
		}
	}
	if _, b = do(ad, "GET", u, ""); !strings.Contains(string(b), `"user_price":"0.00"`) {
		t.Fatalf("%s", b)
	}
	// set prices
	cfg := `{"currency":"SEK","user_price":"50","gb_price":"2","vat_percent":"25","profiles":[{"key":"kund.example.com","customer":"Kund Holding AB","note":"Faktureras kvartalsvis"}]}`
	if r, b := do(ad, "PUT", u, cfg); r.StatusCode != 200 || !strings.Contains(string(b), `"user_price":"50.00"`) {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	// the report of October: 12 users × 50 + 33.333 GB × 2 = 600.00 + 66.67
	r, b := do(ad, "GET", u+"/report?period=2026-10", "")
	var rep billing.Report
	json.Unmarshal(b, &rep)
	if r.StatusCode != 200 || !rep.Complete || len(rep.Invoices) != 1 {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	inv := rep.Invoices[0]
	if inv.Customer != "Kund Holding AB" || inv.Currency != "SEK" || inv.Net != 66667 || inv.VAT != 16667 || inv.Total != 83334 || inv.Lines[0].Name != "kund-ab" {
		t.Fatalf("%+v", inv)
	}
	// files for a spreadsheet
	r, b = do(ad, "GET", u+"/report?period=2026-10&format=csv&view=invoices&locale=sv", "")
	if r.Header.Get("Content-Type") != "text/csv; charset=utf-8" || !strings.Contains(r.Header.Get("Content-Disposition"), "lumen-billing-2026-10-invoices.csv") ||
		!strings.Contains(string(b), "666,67") || !strings.Contains(string(b), "Kund Holding AB") {
		t.Fatalf("%v %q", r.Header, b)
	}
	if _, b = do(ad, "GET", u+"/report?period=2026-10&format=csv", ""); !strings.HasPrefix(string(b), "period,customer,instance,") || !strings.Contains(string(b), "600.00") {
		t.Fatalf("%q", b)
	}
	if c := code(ad, "GET", u+"/report?period=10-2026", ""); c != 400 {
		t.Fatalf("bad period: %d", c)
	}
	// the same days asked for as a range: the same amounts, and the file is named after the days
	var rr billing.Report
	r, b = do(ad, "GET", u+"/report?from=2026-10-01&to=2026-10-31", "")
	json.Unmarshal(b, &rr)
	if r.StatusCode != 200 || len(rr.Invoices) != 1 || rr.Invoices[0].Net != 66667 || rr.Period != "2026-10-01 to 2026-10-31" {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	if r, _ = do(ad, "GET", u+"/report?from=2026-10-01&to=2026-10-31&format=csv", ""); !strings.Contains(r.Header.Get("Content-Disposition"), "lumen-billing-2026-10-01_2026-10-31-lines.csv") {
		t.Fatalf("%v", r.Header)
	}
	for _, q := range []string{"from=2026-10-31&to=2026-10-01", "from=2026-10-01", "from=x&to=y"} {
		if c := code(ad, "GET", u+"/report?"+q, ""); c != 400 {
			t.Fatalf("%s: %d", q, c)
		}
	}
	// another tenant sees neither the prices nor the usage
	if _, b = do(gx, "GET", u, ""); strings.Contains(string(b), `"currency":"SEK"`) || strings.Contains(string(b), "kund.example.com") {
		t.Fatalf("%s", b)
	}
	if _, b = do(gx, "GET", u+"/report?period=2026-10", ""); strings.Contains(string(b), "Kund") {
		t.Fatalf("%s", b)
	}
	// the change of prices is in the log of changes, and the prices are not in it
	var found bool
	for _, e := range al.List("acme", 50) {
		if e.Action == "billing.update" {
			found = true
			if strings.Contains(e.Summary, "50") {
				t.Fatalf("%+v", e)
			}
		}
	}
	if !found {
		t.Fatal("changing the prices must be recorded")
	}
}
