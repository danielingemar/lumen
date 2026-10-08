package store

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielingemar/lumen/internal/config"
)

func TestClickHouseHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		q := r.URL.Query().Get("query")
		switch {
		case strings.Contains(q, "system.disks"):
			// ClickHouse writes 64-bit numbers as strings in JSON
			io.WriteString(w, `{"name":"default","free":"5368709120","total":"75161927680"}`+"\n")
		case strings.Contains(q, "system.parts"):
			io.WriteString(w, `{"name":"default.otel_logs","bytes":"9663676416"}`+"\n"+`{"name":"system.query_log","bytes":"3221225472"}`+"\n")
		default:
			http.Error(w, "unexpected: "+q, 500)
		}
	}))
	defer srv.Close()
	c := NewClickHouse(config.Config{ClickHouseURL: srv.URL, ClickHouseDB: "lumen", ClickHouseUser: "default"})
	h, err := c.Health(context.Background())
	if err != nil || len(h.Disks) != 1 || h.Disks[0].Name != "default" || h.Disks[0].Free != 5368709120 || h.Disks[0].Total != 75161927680 {
		t.Fatalf("%+v %v", h, err)
	}
	if u := h.Disks[0].UsedPct(); u < 92.8 || u > 93.0 {
		t.Fatalf("used: %v", u)
	}
	if len(h.Tables) != 2 || h.Tables[0].Name != "default.otel_logs" || h.Tables[0].Bytes != 9663676416 || h.Tables[1].Name != "system.query_log" {
		t.Fatalf("the biggest tables, system tables included: %+v", h.Tables)
	}
	// a failing ClickHouse is an error the page can show
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "Code: 999", 500) }))
	defer bad.Close()
	if _, err := NewClickHouse(config.Config{ClickHouseURL: bad.URL, ClickHouseDB: "lumen"}).Health(context.Background()); err == nil {
		t.Fatal("an error")
	}
}
