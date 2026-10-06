package store

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/danielingemar/lumen/internal/config"
)

func TestPurgeAndCountSendTheRightStatements(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	var settings []string
	rows := `{"n":0}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		q := r.URL.Query()
		mu.Lock()
		seen = append(seen, q.Get("query")+" | tenant="+q.Get("param_tenant"))
		settings = append(settings, q.Get("mutations_sync"))
		mu.Unlock()
		if strings.HasPrefix(q.Get("query"), "SELECT") {
			io.WriteString(w, rows+"\n")
		}
	}))
	defer srv.Close()
	c := NewClickHouse(config.Config{ClickHouseURL: srv.URL, ClickHouseDB: "lumen", ClickHouseUser: "default"})
	if err := c.PurgeTenant(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 6 {
		t.Fatalf("every live and archive table: %v", seen)
	}
	want := map[string]bool{}
	for _, tb := range []string{"otel_spans", "otel_logs", "otel_metrics"} {
		want["ALTER TABLE "+tb+" DELETE WHERE tenant = {tenant:String} | tenant=acme"] = true
		want["ALTER TABLE "+tb+"_archive DELETE WHERE tenant = {tenant:String} | tenant=acme"] = true
	}
	for i, s := range seen {
		if !want[s] {
			t.Errorf("unexpected statement %q", s)
		}
		delete(want, s)
		if settings[i] != "1" {
			t.Errorf("the purge must wait until it is done (mutations_sync=1), got %q", settings[i])
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing: %v", want)
	}
	if strings.Contains(strings.Join(seen, " "), "acme'") {
		t.Fatal("the tenant name is a parameter, never part of the SQL text")
	}
	if err := c.PurgeTenant(context.Background(), "  "); err == nil {
		t.Fatal("a purge with no tenant name must be refused")
	}
	rows = `{"n":12}`
	if n, err := c.TenantRows(context.Background(), "acme"); err != nil || n != 12 {
		t.Fatalf("%d %v", n, err)
	}
	if last := seen[len(seen)-1]; !strings.Contains(last, "tenant=acme") || !strings.Contains(last, "otel_spans_archive") || !strings.Contains(last, "otel_metrics WHERE") {
		t.Fatalf("it counts live and archive tables: %s", last)
	}
	// a failing ClickHouse is an error, not a silent success
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "Code: 999", 500) }))
	defer bad.Close()
	c2 := NewClickHouse(config.Config{ClickHouseURL: bad.URL, ClickHouseDB: "lumen"})
	if err := c2.PurgeTenant(context.Background(), "acme"); err == nil || !strings.Contains(err.Error(), "purging otel_spans") {
		t.Fatalf("%v", err)
	}
}
