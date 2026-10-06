package audit

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
)

func TestAuditLog(t *testing.T) {
	db, _ := docstore.OpenFile(t.TempDir())
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	l := New(db)
	l.Now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		now = now.Add(time.Minute)
		if err := l.Add(Entry{Tenant: "acme", Actor: "ops", Action: "support.enter", Target: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	l.Add(Entry{Tenant: "globex", Actor: "ops", Action: "tenant.suspend", Detail: strings.Repeat("x", 1000)})
	if got := l.List("acme", 0); len(got) != 5 || got[0].Target != "4" || got[4].Target != "0" {
		t.Fatalf("newest first, and only the tenant's own: %+v", got)
	}
	if got := l.List("acme", 2); len(got) != 2 {
		t.Fatal("limit")
	}
	if got := l.List("globex", 0); len(got) != 1 || len(got[0].Detail) != 400 {
		t.Fatalf("a long detail is cut: %+v", got)
	}
	if got := l.List("", 0); len(got) != 6 {
		t.Fatalf("the operator sees everything: %d", len(got))
	}
	if got := l.List("nobody", 0); len(got) != 0 {
		t.Fatal("a tenant with no entries")
	}
	l.Prune(3)
	if got := l.List("acme", 0); len(got) != 3 || got[0].Target != "4" || got[2].Target != "2" {
		t.Fatalf("prune keeps the newest per tenant: %+v", got)
	}
	if len(l.List("globex", 0)) != 1 {
		t.Fatal("another tenant's entries are not pruned by acme's")
	}
}
