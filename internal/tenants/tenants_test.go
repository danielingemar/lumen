package tenants

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func newSvc(t *testing.T) (*Service, docstore.Backend, *time.Time) {
	db, err := docstore.OpenFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := t0
	s := New(db)
	s.Now = func() time.Time { return now }
	return s, db, &now
}

func TestCreateAndValidate(t *testing.T) {
	s, _, _ := newSvc(t)
	for _, bad := range []string{"", "a", "Acme", "acme corp", "-acme", "acme-", "a_b", "operator", strings.Repeat("a", 41), "ac/me", "../x"} {
		if err := ValidID(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q must be refused: %v", bad, err)
		}
	}
	for _, good := range []string{"ac", "acme", "acme-ab", "a1", "customer-2026"} {
		if err := ValidID(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	tn, err := s.Create(CreateIn{ID: "acme", Name: "ACME AB", Contact: "ops@acme.example", Quotas: Quotas{Users: 5}})
	if err != nil || tn.Status != Active || tn.SupportAccess != SupportAsk || tn.SuspendIngest != "reject" || tn.Quotas.Users != 5 || tn.Migrated {
		t.Fatalf("%+v %v", tn, err)
	}
	if _, err := s.Create(CreateIn{ID: "acme", Name: "again"}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("%v", err)
	}
	for name, in := range map[string]CreateIn{
		"no name": {ID: "x1"}, "multiline": {ID: "x2", Name: "a\nb"}, "negative limit": {ID: "x3", Name: "n", Quotas: Quotas{Hosts: -1}},
		"bad support": {ID: "x4", Name: "n", SupportAccess: "sometimes"}, "long notes": {ID: "x5", Name: "n", Notes: strings.Repeat("a", 2001)},
	} {
		if _, err := s.Create(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	got, ok := s.Get("acme")
	if !ok || got.Name != "ACME AB" {
		t.Fatal("get")
	}
	if _, ok := s.Get("nobody"); ok {
		t.Fatal("unknown")
	}
	// updates
	u, err := s.Update("acme", UpdateIn{Name: "ACME Group", Contact: "x", Plan: "pro", Quotas: Quotas{Hosts: 10, Hard: true}, SuspendIngest: "drop"})
	if err != nil || u.Name != "ACME Group" || !u.Quotas.Hard || u.SuspendIngest != "drop" || u.Plan != "pro" {
		t.Fatalf("%+v %v", u, err)
	}
	if _, err := s.Update("acme", UpdateIn{Name: "x", SuspendIngest: "explode"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("suspend mode")
	}
	if _, err := s.Update("nobody", UpdateIn{Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatal("not found")
	}
}

func TestSuspendResumeAndTheOperatorTenant(t *testing.T) {
	s, _, now := newSvc(t)
	s.Ensure([]string{"operator", "acme"})
	if op, _ := s.Get("operator"); !op.Reserved || op.SupportAccess != SupportOff {
		t.Fatalf("%+v", op)
	}
	if _, err := s.SetStatus("operator", Suspended, ""); !errors.Is(err, ErrInvalid) {
		t.Fatal("the operator tenant cannot be suspended: that would lock out everyone who runs the installation")
	}
	tn, err := s.SetStatus("acme", Suspended, "unpaid invoice")
	if err != nil || tn.Status != Suspended || tn.SuspendedReason != "unpaid invoice" || !tn.SuspendedAt.Equal(t0) {
		t.Fatalf("%+v %v", tn, err)
	}
	*now = t0.Add(time.Hour)
	if again, _ := s.SetStatus("acme", Suspended, "later reason"); again.SuspendedReason != "unpaid invoice" || !again.SuspendedAt.Equal(t0) {
		t.Fatal("suspending twice keeps the first reason and time")
	}
	if g, _ := s.Get("acme"); g.Status != Suspended {
		t.Fatal("the cache sees changes made through the service")
	}
	if r, err := s.SetStatus("acme", Active, ""); err != nil || r.Status != Active || r.SuspendedReason != "" || !r.SuspendedAt.IsZero() {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := s.SetStatus("acme", "weird", ""); !errors.Is(err, ErrInvalid) {
		t.Fatal("status")
	}
	if _, err := s.SetStatus("acme", Suspended, "a\nb"); !errors.Is(err, ErrInvalid) {
		t.Fatal("reason")
	}
}

func TestEnsureAndDiscoverExistingTenants(t *testing.T) {
	s, db, _ := newSvc(t)
	put := func(coll, id, tenant string) {
		c, cancel := ctx5()
		defer cancel()
		db.Put(c, coll, id, map[string]any{"name": id, "tenant": tenant}, "")
	}
	put("users", "anna", "acme")
	put("users", "boss", "globex")
	put("keys", "k1", "acme")
	put("dashboards", "d1", "initech")
	put("hosts", "initech:web1", "initech")
	put("instances", "i1", "globex")
	put("users", "ops", "operator")
	found := Discover(db)
	if strings.Join(found, ",") != "acme,globex,initech,operator" {
		t.Fatalf("every tenant that appears anywhere: %v", found)
	}
	if n := s.Ensure(append(found, "main", "", "acme")); n != 5 {
		t.Fatalf("5 new records (blank and duplicate names ignored): %d", n)
	}
	if n := s.Ensure(found); n != 0 {
		t.Fatalf("running it again changes nothing: %d", n)
	}
	a, _ := s.Get("acme")
	if !a.Migrated || a.SupportAccess != SupportAllow || a.Status != Active || a.Name != "acme" {
		t.Fatalf("a tenant that existed before gets sensible defaults: %+v", a)
	}
	if list := s.List(); len(list) != 5 || list[0].ID != "operator" || list[1].ID != "acme" {
		t.Fatalf("the operator tenant first, then by name: %v", list)
	}
	if s.Count("acme", "users") != 1 || s.Count("globex", "instances") != 1 || s.Count("acme", "instances") != 0 || s.Count("nobody", "users") != 0 {
		t.Fatal("counts per tenant")
	}
}

func TestSupportAccessConsent(t *testing.T) {
	s, _, now := newSvc(t)
	s.Create(CreateIn{ID: "acme", Name: "ACME"}) // default: ask
	s.Create(CreateIn{ID: "open", Name: "Open", SupportAccess: "allow"})
	s.Create(CreateIn{ID: "closed", Name: "Closed", SupportAccess: "off"})
	if err := s.CanEnter("open"); err != nil {
		t.Fatal(err)
	}
	if err := s.CanEnter("closed"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "switched support access off") {
		t.Fatalf("%v", err)
	}
	if err := s.CanEnter("acme"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "granted access") {
		t.Fatalf("with 'ask' nobody gets in until an administrator allows it: %v", err)
	}
	for name, d := range map[string]time.Duration{"too short": time.Minute, "too long": 8 * 24 * time.Hour} {
		if _, err := s.GrantAccess("acme", "boss", d); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	g, err := s.GrantAccess("acme", "boss", 2*time.Hour)
	if err != nil || !g.Until.Equal(t0.Add(2*time.Hour)) || g.By != "boss" {
		t.Fatalf("%+v %v", g, err)
	}
	if err := s.CanEnter("acme"); err != nil {
		t.Fatalf("with a grant: %v", err)
	}
	*now = t0.Add(3 * time.Hour)
	if err := s.CanEnter("acme"); err == nil {
		t.Fatal("the grant has run out")
	}
	s.GrantAccess("acme", "boss", time.Hour)
	s.Revoke("acme")
	if err := s.CanEnter("acme"); err == nil {
		t.Fatal("a revoked grant is gone")
	}
	s.GrantAccess("acme", "boss", time.Hour)
	if _, err := s.SetSupportAccess("acme", SupportOff); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ActiveGrant("acme"); ok {
		t.Fatal("switching access off ends a grant")
	}
	if _, err := s.SetSupportAccess("acme", "maybe"); !errors.Is(err, ErrInvalid) {
		t.Fatal("mode")
	}
	if err := s.CanEnter("nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown tenant")
	}
	s.SetStatus("open", Suspended, "x")
	if err := s.CanEnter("open"); err != nil {
		t.Fatal("an operator can go into a suspended tenant to help sort things out")
	}
}

func TestWarningsAndBytes(t *testing.T) {
	q := Quotas{Hosts: 10, Users: 5, IngestBytesPerDay: 1 << 30}
	if w := q.Warnings(Usage{Hosts: 3, Users: 2, IngestBytesToday: 1 << 20}); len(w) != 0 {
		t.Fatalf("%v", w)
	}
	w := q.Warnings(Usage{Hosts: 9, Users: 6, IngestBytesToday: 2 << 30})
	if len(w) != 3 || !strings.Contains(w[0], "9 of 10 hosts") || !strings.Contains(w[1], "6 users are in use, but the limit is 5") || !strings.Contains(w[2], "over the daily limit") {
		t.Fatalf("%v", w)
	}
	if w := (Quotas{}).Warnings(Usage{Hosts: 1000}); len(w) != 0 {
		t.Fatal("no limits, no warnings")
	}
	for in, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1 KB", 1536: "1.5 KB", 5 << 20: "5 MB", 3 << 30: "3 GB"} {
		if got := HumanBytes(in); got != want {
			t.Errorf("%d: %q want %q", in, got, want)
		}
	}
}

// ---- offboarding ----

type fakePurger struct {
	purged []string
	rows   map[string]int64
	err    error
	left   int64
}

func (f *fakePurger) PurgeTenant(_ context.Context, tenant string) error {
	if f.err != nil {
		return f.err
	}
	f.purged = append(f.purged, tenant)
	f.rows[tenant] = f.left
	return nil
}
func (f *fakePurger) TenantRows(_ context.Context, tenant string) (int64, error) {
	return f.rows[tenant], nil
}

func seed(t *testing.T, db docstore.Backend, tenants ...string) {
	for _, tn := range tenants {
		for _, coll := range append(append([]string{}, PurgeCollections...), "usage", "audit") {
			c, cancel := ctx5()
			if err := db.Put(c, coll, coll+"-"+tn, map[string]any{"id": coll + "-" + tn, "tenant": tn, "hash": "SECRET-HASH", "secrets_enc": map[string]string{"token": "SEALED"}, "token_enc": "SEALED2", "name": coll}, ""); err != nil {
				t.Fatal(err)
			}
			cancel()
		}
	}
}

func TestOffboardingRemovesEverythingOfOneTenantOnly(t *testing.T) {
	s, db, _ := newSvc(t)
	s.Create(CreateIn{ID: "acme", Name: "ACME", Contact: "ops@acme.example", Notes: "private notes"})
	s.Create(CreateIn{ID: "globex", Name: "Globex"})
	seed(t, db, "acme", "globex")
	backup := t.TempDir()
	for _, p := range []string{"telemetry/2026-10-05/acme", "telemetry/2026-10-06/acme", "telemetry/2026-10-06/globex", "config"} {
		os.MkdirAll(filepath.Join(backup, p), 0o755)
		os.WriteFile(filepath.Join(backup, p, "x.jsonl.gz"), []byte("data"), 0o600)
	}
	pg := &fakePurger{rows: map[string]int64{"acme": 500, "globex": 900}}
	o := &Offboarder{Svc: s, Purger: pg, BackupDir: backup}
	if err := o.Begin("acme", "wrong"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "give its id") {
		t.Fatalf("a removal must be confirmed with the id: %v", err)
	}
	if err := o.Begin("operator", "operator"); err == nil {
		t.Fatal("operator tenant")
	}
	if err := o.Begin("nobody", "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatal("not found")
	}
	if err := o.Begin("acme", "acme"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("acme"); got.Status != Offboarding || got.Offboard == nil || got.Offboard.State != "purging" || len(got.Offboard.Steps) != 4 {
		t.Fatalf("%+v", got)
	}
	if err := o.Begin("acme", "acme"); err == nil {
		t.Fatal("it cannot be started twice at the same time")
	}
	o.Run(context.Background(), "acme")
	got, _ := s.Load2("acme")
	if got.Status != Deleted || got.Offboard.State != "done" || got.Contact != "" || got.Notes != "" || got.Name != "ACME" || got.Deleted.IsZero() {
		t.Fatalf("a tombstone: the name stays, personal details go: %+v", got)
	}
	for _, st := range got.Offboard.Steps {
		if !st.Done || st.Detail == "" {
			t.Fatalf("every step reports what it did: %+v", got.Offboard.Steps)
		}
	}
	if len(pg.purged) != 1 || pg.purged[0] != "acme" {
		t.Fatalf("telemetry is purged for the one tenant only: %v", pg.purged)
	}
	for _, coll := range PurgeCollections {
		if s.Count("acme", coll) != 0 {
			t.Errorf("%s still has documents of acme", coll)
		}
		if s.Count("globex", coll) != 1 {
			t.Errorf("%s: globex lost a document", coll)
		}
	}
	if s.Count("acme", "usage") != 1 || s.Count("acme", "audit") != 1 {
		t.Fatal("what the tenant used (for invoices) and the access log are kept")
	}
	if _, err := os.Stat(filepath.Join(backup, "telemetry/2026-10-05/acme")); !os.IsNotExist(err) {
		t.Fatal("backups of the tenant are removed")
	}
	for _, keep := range []string{"telemetry/2026-10-06/globex", "config"} {
		if _, err := os.Stat(filepath.Join(backup, keep, "x.jsonl.gz")); err != nil {
			t.Errorf("%s must be untouched: %v", keep, err)
		}
	}
	if !strings.Contains(got.Offboard.Steps[2].Detail, "configuration dumps") {
		t.Fatalf("the operator is told that the configuration dumps still hold the settings: %q", got.Offboard.Steps[2].Detail)
	}
	if _, err := s.Create(CreateIn{ID: "acme", Name: "Imposter"}); err == nil || !strings.Contains(err.Error(), "cannot be used again") {
		t.Fatalf("the name stays taken: %v", err)
	}
	if err := s.CanEnter("acme"); !errors.Is(err, ErrNotFound) {
		t.Fatal("nobody can go into a removed tenant")
	}
	if _, err := s.Update("acme", UpdateIn{Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatal("nor change it")
	}
}

func (s *Service) Load2(id string) (Tenant, bool) { return s.load(id) }

func TestOffboardingFailsLoudlyAndCanBeRepeated(t *testing.T) {
	s, db, _ := newSvc(t)
	s.Create(CreateIn{ID: "acme", Name: "ACME"})
	seed(t, db, "acme")
	pg := &fakePurger{rows: map[string]int64{}, err: errors.New("clickhouse is down")}
	o := &Offboarder{Svc: s, Purger: pg}
	o.Begin("acme", "acme")
	o.Run(context.Background(), "acme")
	got, _ := s.Load2("acme")
	if got.Status != Offboarding || got.Offboard.State != "failed" || !strings.Contains(got.Offboard.Error, "telemetry: clickhouse is down") || !got.Offboard.Steps[0].Done || got.Offboard.Steps[1].Done {
		t.Fatalf("a failure says which step and why, and what was already done: %+v", got.Offboard)
	}
	// data still arrives for the tenant: the final check refuses to say it is gone
	pg.err, pg.left = nil, 42
	if err := o.Begin("acme", "acme"); err != nil {
		t.Fatalf("a failed removal can be started again: %v", err)
	}
	o.Run(context.Background(), "acme")
	got, _ = s.Load2("acme")
	if got.Offboard.State != "failed" || !strings.Contains(got.Offboard.Error, "42 telemetry rows are still there") || got.Status == Deleted {
		t.Fatalf("it must not claim success while rows remain: %+v", got.Offboard)
	}
	pg.left = 0
	o.Begin("acme", "acme")
	o.Run(context.Background(), "acme")
	if got, _ = s.Load2("acme"); got.Status != Deleted || got.Offboard.State != "done" {
		t.Fatalf("%+v", got)
	}
	// a tenant name that is not safe as a directory name does not make the removal touch other directories
	s.Ensure([]string{"../etc"})
	backup := t.TempDir()
	os.MkdirAll(filepath.Join(backup, "telemetry/2026-10-06/x"), 0o755)
	o2 := &Offboarder{Svc: s, Purger: &fakePurger{rows: map[string]int64{}}, BackupDir: backup}
	o2.Begin("../etc", "../etc")
	o2.Run(context.Background(), "../etc")
	g2, _ := s.Load2("../etc")
	if !strings.Contains(g2.Offboard.Steps[2].Detail, "not touched") {
		t.Fatalf("%q", g2.Offboard.Steps[2].Detail)
	}
	if _, err := os.Stat(filepath.Join(backup, "telemetry/2026-10-06/x")); err != nil {
		t.Fatal("other backups are untouched")
	}
}

func TestExportLeavesOutSecrets(t *testing.T) {
	s, db, _ := newSvc(t)
	s.Create(CreateIn{ID: "acme", Name: "ACME"})
	seed(t, db, "acme", "globex")
	var buf bytes.Buffer
	if err := s.Export(&buf, "acme"); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		names[f.Name] = string(b)
	}
	for _, want := range []string{"tenant.json", "users.json", "dashboards.json", "alert_channels.json", "usage.json", "audit.json", "README.txt"} {
		if _, ok := names[want]; !ok {
			t.Errorf("the export has %s", want)
		}
	}
	all := ""
	for _, v := range names {
		all += v
	}
	for _, secret := range []string{"SECRET-HASH", "SEALED", "SEALED2"} {
		if strings.Contains(all, secret) {
			t.Fatalf("%q must never be exported", secret)
		}
	}
	if strings.Contains(all, "globex") {
		t.Fatal("only the tenant's own documents")
	}
	if !strings.Contains(names["README.txt"], "do not") && !strings.Contains(names["README.txt"], "does not contain traces") {
		t.Fatalf("%s", names["README.txt"])
	}
	if err := s.Export(io.Discard, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown")
	}
	_ = fmt.Sprint
}
