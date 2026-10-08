package billing

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
)

func f(v float64) *float64 { return &v }

func TestDecimalsAreParsedAndPrintedExactly(t *testing.T) {
	for in, want := range map[string]string{"49": "49.00", "49.5": "49.50", "49,50": "49.50", "0.125": "0.125", "0.0001": "0.0001", "": "0.00", " 7 ": "7.00", "100": "100.00"} {
		d, err := ParseDec(in)
		if err != nil || d.String() != want {
			t.Errorf("%q -> %v %v, want %s", in, d, err, want)
		}
	}
	for _, bad := range []string{"-1", "1.23456", "abc", "1e3", "1.2.3", "9999999999", "1 000"} {
		if _, err := ParseDec(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestAmountsAreRoundedHalfUpOnce(t *testing.T) {
	// 12 users × 49.50 = 594.00
	if got := amount(12000, mustDec("49.50"), 2, 1, 1); got != 59400 {
		t.Fatal(got)
	}
	// 3.333 GB × 0.35 = 1.16655 -> 1.17
	if got := amount(3333, mustDec("0.35"), 2, 1, 1); got != 117 {
		t.Fatal(got)
	}
	// exactly half a cent goes up: 0.5 × 0.01 = 0.005 -> 0.01
	if got := amount(500, mustDec("0.01"), 2, 1, 1); got != 1 {
		t.Fatal(got)
	}
	// no decimals (yen): 2.5 × 3 = 7.5 -> 8
	if got := amount(2500, mustDec("3"), 0, 1, 1); got != 8 {
		t.Fatal(got)
	}
	// 10 of 30 days: 30 users × 100 × 10/30 = 1000.00
	if got := amount(30000, mustDec("100"), 2, 10, 30); got != 100000 {
		t.Fatal(got)
	}
	if amount(0, mustDec("5"), 2, 1, 1) != 0 || amount(1000, 0, 2, 1, 1) != 0 {
		t.Fatal("nothing used or nothing priced is nothing")
	}
	// a big instance does not overflow: 5 PB at 0.35 per GB
	if got := amount(5_000_000_000, mustDec("0.35"), 2, 1, 1); got != 175_000_000 {
		t.Fatal(got)
	}
	if percentOf(10000, mustDec("25")) != 2500 || percentOf(1, mustDec("50")) != 1 || percentOf(1, mustDec("49")) != 0 {
		t.Fatal("percent")
	}
	if Minor(12345, 2) != "123.45" || Minor(5, 2) != "0.05" || Minor(-250, 2) != "-2.50" || Minor(7, 0) != "7" {
		t.Fatal("minor")
	}
}

func mustDec(s string) Dec { d, _ := ParseDec(s); return d }

func TestEveryCurrencyIsUsable(t *testing.T) {
	for _, code := range []string{"SEK", "EUR", "USD", "GBP", "NOK", "DKK", "CHF", "JPY"} {
		if _, ok := Lookup(code); !ok {
			t.Errorf("%s is missing", code)
		}
	}
	if c, _ := Lookup("JPY"); c.Decimals != 0 {
		t.Fatal("yen has no minor unit")
	}
	seen := map[string]bool{}
	for _, c := range Currencies {
		if seen[c.Code] || len(c.Code) != 3 || c.Name == "" {
			t.Errorf("bad entry %+v", c)
		}
		seen[c.Code] = true
	}
}

func TestConfigIsCheckedAndCleaned(t *testing.T) {
	c, err := Config{Currency: "SEK", UserPrice: "49,5", GBPrice: "0.35", VAT: "25"}.Clean()
	if err != nil || c.UserPrice != "49.50" || c.VAT != "25.00" || c.UserBasis != Peak || c.GBUnit != "gb" {
		t.Fatalf("%+v %v", c, err)
	}
	bad := []Config{
		{Currency: "XXX"}, {UserPrice: "-5"}, {GBPrice: "abc"}, {VAT: "101"}, {UserBasis: "median"}, {GBUnit: "tb"},
		{Profiles: []Profile{{Key: "a b"}}}, {Profiles: []Profile{{Key: "a"}, {Key: "a"}}},
		{Currency: "SEK", Profiles: []Profile{{Key: "nc.example.com", Currency: "EUR"}}},                 // another currency needs both prices
		{Currency: "SEK", Profiles: []Profile{{Key: "nc.example.com", Currency: "EUR", UserPrice: "5"}}}, // ...both
		{Profiles: []Profile{{Key: "nc.example.com", Discount: "150"}}}, {Profiles: []Profile{{Key: "k", VAT: "x"}}},
	}
	for i, b := range bad {
		if _, err := b.Clean(); err == nil {
			t.Errorf("case %d should be refused: %+v", i, b)
		}
	}
	ok, err := Config{Currency: "SEK", Profiles: []Profile{{Key: "nc.example.com:8443", Currency: "EUR", UserPrice: "4", GBPrice: "0.03"}, {Key: "b", Currency: "SEK"}}}.Clean()
	if err != nil || ok.Profiles[0].Currency != "" || ok.Profiles[0].Key != "b" || ok.Profiles[1].Currency != "EUR" {
		t.Fatalf("a customer in the default currency has no override, and profiles are sorted: %+v %v", ok.Profiles, err)
	}
}

func TestServiceKeepsTheConfigurationPerTenant(t *testing.T) {
	db, err := docstore.OpenFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(db)
	if g := s.Get("acme"); g.UserPrice != "0.00" || g.Currency != "EUR" {
		t.Fatalf("nothing is charged until prices are set: %+v", g)
	}
	if _, err := s.Put("acme", Config{Currency: "SEK", UserPrice: "50"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put("acme", Config{Currency: "ZZZ"}); err == nil {
		t.Fatal("refused")
	}
	if g := s.Get("acme"); g.Currency != "SEK" || g.UserPrice != "50.00" {
		t.Fatalf("%+v", g)
	}
	if g := s.Get("globex"); g.Currency != "EUR" {
		t.Fatal("another tenant must not see it")
	}
}

var oct = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC) // October 2026 is over

func days(from, to int, u, gb float64) map[string]DayUsage {
	m := map[string]DayUsage{}
	for d := from; d <= to; d++ {
		m[time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC).Format("2006-01-02")] = DayUsage{Users: f(u), Bytes: f(gb * 1e9)}
	}
	return m
}

func TestReportChargesUsersAndData(t *testing.T) {
	cfg := Config{Currency: "SEK", UserPrice: "50", GBPrice: "2", VAT: "25"}
	months := []Month{{Instance: "kund.example.com", Month: "2026-10", Days: days(1, 31, 10, 100)}}
	r, err := Build(cfg, "2026-10", months, []Known{{"kund.example.com", "Kund AB"}}, oct)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Complete || len(r.Invoices) != 1 || len(r.Totals) != 1 {
		t.Fatalf("%+v", r)
	}
	in := r.Invoices[0]
	l := in.Lines[0]
	// 10 × 50 = 500.00 and 100 GB × 2 = 200.00
	if in.Customer != "Kund AB" || l.UserAmount != 50000 || l.DataAmount != 20000 || in.Net != 70000 || in.VAT != 17500 || in.Total != 87500 {
		t.Fatalf("%+v", in)
	}
	if r.Totals[0].Total != 87500 || r.Totals[0].Currency != "SEK" {
		t.Fatalf("%+v", r.Totals)
	}
	if len(l.Warnings) != 0 {
		t.Fatalf("%v", l.Warnings)
	}
}

func TestBasisAverageEndAndPeak(t *testing.T) {
	d := map[string]DayUsage{"2026-10-01": {Users: f(10), Bytes: f(10e9)}, "2026-10-02": {Users: f(20), Bytes: f(30e9)}, "2026-10-03": {Users: f(15), Bytes: f(20e9)}}
	m := []Month{{Instance: "a", Month: "2026-10", Days: d}}
	for basis, want := range map[string][2]float64{Peak: {20, 30}, Average: {15, 20}, End: {15, 20}} {
		r, _ := Build(Config{UserBasis: basis, DataBasis: basis, UserPrice: "1", GBPrice: "1"}, "2026-10", m, nil, oct)
		l := r.Invoices[0].Lines[0]
		if l.Users != want[0] || l.GB != want[1] {
			t.Errorf("%s: %v %v, want %v", basis, l.Users, l.GB, want)
		}
	}
	// 2^30 bytes is one GiB
	d2 := []Month{{Instance: "a", Month: "2026-10", Days: map[string]DayUsage{"2026-10-01": {Users: f(1), Bytes: f(1 << 30)}}}}
	r, _ := Build(Config{GBUnit: "gib", GBPrice: "1"}, "2026-10", d2, nil, oct)
	if r.Invoices[0].Lines[0].GB != 1 {
		t.Fatalf("%v", r.Invoices[0].Lines[0].GB)
	}
}

func TestProratingChargesOnlyTheDaysReported(t *testing.T) {
	m := []Month{{Instance: "a", Month: "2026-10", Days: days(22, 31, 30, 0)}} // 10 of 31 days
	cfg := Config{UserPrice: "31", Prorate: true}
	r, _ := Build(cfg, "2026-10", m, nil, oct)
	if got := r.Invoices[0].Lines[0].UserAmount; got != 30*3100*10/31 { // 30 × 31.00 × 10/31 = 300.00
		t.Fatalf("%d", got)
	}
	cfg.Prorate = false
	r, _ = Build(cfg, "2026-10", m, nil, oct)
	l := r.Invoices[0].Lines[0]
	if l.UserAmount != 93000 || len(l.Warnings) == 0 || !strings.Contains(strings.Join(l.Warnings, " "), "10 of 31") {
		t.Fatalf("the whole month is charged, and it says so: %+v", l)
	}
}

func TestCustomersGroupInstancesAndCurrenciesStaySeparate(t *testing.T) {
	cfg := Config{Currency: "SEK", UserPrice: "10", GBPrice: "1", VAT: "25", Profiles: []Profile{
		{Key: "a", Customer: "Acme"}, {Key: "b", Customer: "Acme", Discount: "10"},
		{Key: "c", Customer: "Nordic", Currency: "EUR", UserPrice: "1", GBPrice: "0.1", VAT: "0"},
		{Key: "own", Exclude: true},
		{Key: "d", Customer: "Acme", Currency: "EUR", UserPrice: "1", GBPrice: "1"},
	}}
	one := func(k string) Month { return Month{Instance: k, Month: "2026-10", Days: days(1, 31, 10, 0)} }
	r, err := Build(cfg, "2026-10", []Month{one("a"), one("b"), one("c"), one("own"), one("d")}, nil, oct)
	if err != nil {
		t.Fatal(err)
	}
	if r.Excluded != 1 || len(r.Invoices) != 3 {
		t.Fatalf("%d invoices, %d excluded", len(r.Invoices), r.Excluded)
	}
	acme := r.Invoices[0]
	if acme.Customer != "Acme" || acme.Currency != "EUR" { // same customer, another currency: its own invoice (EUR sorts before SEK)
		t.Fatalf("%+v", acme)
	}
	var sek Invoice
	for _, in := range r.Invoices {
		if in.Customer == "Acme" && in.Currency == "SEK" {
			sek = in
		}
	}
	// a: 100.00; b: 100.00 less 10% = 90.00; net 190.00, VAT 25% = 47.50
	if len(sek.Lines) != 2 || sek.Net != 19000 || sek.VAT != 4750 || sek.Total != 23750 {
		t.Fatalf("%+v", sek)
	}
	if len(r.Totals) != 2 || r.Totals[0].Currency != "EUR" || r.Totals[1].Currency != "SEK" {
		t.Fatalf("each currency is added up alone: %+v", r.Totals)
	}
	for _, in := range r.Invoices {
		if in.Customer == "Nordic" && (in.VAT != 0 || in.Total != 1000) {
			t.Fatalf("a customer with VAT 0: %+v", in)
		}
	}
}

func TestVATOfSeveralRatesOnOneInvoiceIsStatedPerRate(t *testing.T) {
	cfg := Config{UserPrice: "100", VAT: "25", Profiles: []Profile{{Key: "a", Customer: "X"}, {Key: "b", Customer: "X", VAT: "0"}}}
	mm := []Month{{Instance: "a", Month: "2026-10", Days: days(1, 31, 1, 0)}, {Instance: "b", Month: "2026-10", Days: days(1, 31, 1, 0)}}
	r, _ := Build(cfg, "2026-10", mm, nil, oct)
	in := r.Invoices[0]
	if len(in.VATLines) != 2 || in.VAT != 2500 || in.Total != 22500 {
		t.Fatalf("%+v", in)
	}
}

func TestMissingDataIsExplainedNotHidden(t *testing.T) {
	r, _ := Build(Config{UserPrice: "10", GBPrice: "1"}, "2026-10", []Month{
		{Instance: "nostore", Month: "2026-10", Days: map[string]DayUsage{"2026-10-05": {Users: f(4)}}},
		{Instance: "accountsonly", Month: "2026-10", Days: map[string]DayUsage{"2026-10-05": {Accounts: f(7), Bytes: f(1e9)}}},
	}, []Known{{"silent", "Silent"}}, oct)
	got := map[string]Line{}
	for _, in := range r.Invoices {
		for _, l := range in.Lines {
			got[l.Key] = l
		}
	}
	if len(got) != 3 {
		t.Fatalf("an instance that reported nothing is still listed: %v", got)
	}
	if w := strings.Join(got["nostore"].Warnings, " "); !strings.Contains(w, "data used is not reported") || got["nostore"].UserAmount != 4000 {
		t.Fatalf("%+v", got["nostore"])
	}
	if got["accountsonly"].Users != 7 || got["accountsonly"].UsersFrom != "account list" {
		t.Fatalf("the account list stands in for the user count: %+v", got["accountsonly"])
	}
	if w := strings.Join(got["silent"].Warnings, " "); !strings.Contains(w, "Nothing was reported") || got["silent"].Gross != 0 {
		t.Fatalf("%+v", got["silent"])
	}
}

func TestRunningMonthIsMarkedAsNotFinal(t *testing.T) {
	r, _ := Build(Config{}, "2026-10", nil, nil, time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC))
	if r.Complete || len(r.Warnings) == 0 {
		t.Fatalf("%+v", r)
	}
	if _, err := Build(Config{}, "10-2026", nil, nil, oct); err == nil {
		t.Fatal("bad period")
	}
	// days of another month are ignored
	m := []Month{{Instance: "a", Month: "2026-10", Days: map[string]DayUsage{"2026-09-30": {Users: f(99)}, "2026-10-01": {Users: f(2)}}}}
	r, _ = Build(Config{UserPrice: "1"}, "2026-10", m, nil, oct)
	if r.Invoices[0].Lines[0].Users != 2 {
		t.Fatalf("%+v", r.Invoices[0].Lines[0])
	}
}

type fakeSource struct {
	samples []Sample
	calls   int
	from    time.Time
}

func (s *fakeSource) InstanceUsage(ctx context.Context, tenant string, from, to time.Time) ([]Sample, error) {
	s.calls++
	s.from = from
	var out []Sample
	for _, x := range s.samples {
		if tenant == "acme" {
			out = append(out, x)
		}
	}
	return out, nil
}

func TestRecorderKeepsDaysAfterTheTelemetryIsGone(t *testing.T) {
	db, _ := docstore.OpenFile(t.TempDir())
	src := &fakeSource{samples: []Sample{
		{"a", "2026-10-07", DayUsage{Users: f(5), Bytes: f(2e9)}},
		{"a", "2026-10-08", DayUsage{Users: f(6)}},
		{"b", "2026-10-08", DayUsage{Users: f(1)}},
		{"", "2026-10-08", DayUsage{Users: f(1)}},
	}}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	rec := &Recorder{DB: db, Src: src, Tenants: func() []string { return []string{"acme", "globex"} }, Now: func() time.Time { return now }, Backfill: 35 * 24 * time.Hour}
	rec.Pass(context.Background())
	if want := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC); !src.from.Equal(want) {
		t.Fatalf("the first pass reaches back as far as the telemetry: %v", src.from)
	}
	ms, err := Months(context.Background(), db, "acme", "2026-10")
	if err != nil || len(ms) != 2 {
		t.Fatalf("%v %v", ms, err)
	}
	if g, _ := Months(context.Background(), db, "globex", "2026-10"); len(g) != 0 {
		t.Fatal("another tenant has nothing")
	}
	// the next pass reads two days, and a day that is read again replaces the old value
	src.samples = []Sample{{"a", "2026-10-08", DayUsage{Users: f(9), Bytes: f(3e9)}}}
	now = now.Add(time.Hour)
	rec.Pass(context.Background())
	if want := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC); !src.from.Equal(want) {
		t.Fatalf("later passes read two days: %v", src.from)
	}
	ms, _ = Months(context.Background(), db, "acme", "2026-10")
	for _, m := range ms {
		if m.Instance == "a" {
			if *m.Days["2026-10-08"].Users != 9 || *m.Days["2026-10-07"].Users != 5 {
				t.Fatalf("a day is replaced, the others stay: %+v", m.Days)
			}
		}
	}
	// refreshing on request is limited to once a minute
	c := src.calls
	rec.Refresh(context.Background(), "acme")
	rec.Refresh(context.Background(), "acme")
	if src.calls != c+1 {
		t.Fatalf("calls %d -> %d", c, src.calls)
	}
}

func TestCSVForSpreadsheets(t *testing.T) {
	cfg := Config{Currency: "SEK", UserPrice: "49.5", GBPrice: "0.35", Profiles: []Profile{{Key: "a", Customer: "=HYPERLINK(\"x\")", Note: "se; anteckning"}}}
	r, _ := Build(cfg, "2026-10", []Month{{Instance: "a", Month: "2026-10", Days: days(1, 31, 12, 33.3333)}}, nil, oct)
	var b bytes.Buffer
	if err := WriteCSV(&b, r, "lines", ""); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.HasPrefix(out, "period,customer,") || strings.Contains(out, ",=HYPERLINK") || !strings.Contains(out, "'=HYPERLINK") || !strings.Contains(out, "594.00") {
		t.Fatalf("%s", out)
	}
	b.Reset()
	WriteCSV(&b, r, "invoices", "sv")
	sv := b.String()
	if !strings.HasPrefix(sv, "\xef\xbb\xbf") || !strings.Contains(sv, ";SEK;1;") || !strings.Contains(sv, "605,67") || strings.Contains(sv, "605.67") {
		t.Fatalf("%q", sv)
	}
}
