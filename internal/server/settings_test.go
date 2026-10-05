package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/branding"
	"github.com/danielingemar/lumen/internal/dashboards"
	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/model"
	"github.com/danielingemar/lumen/internal/registry"
	"github.com/danielingemar/lumen/internal/secretbox"
)

func settingsServer(t *testing.T) (*httptest.Server, *fakeStore) {
	f, _ := docstore.OpenFile(t.TempDir())
	st, _ := auth.Open(f, 0)
	st.CreateUser("admin", "acme", "admins-long-password")
	st.CreateUserIn("reader", "acme", "readers-long-password", "user")
	g, _ := st.CreateGroup("acme", "LogsOnly", "", map[string]string{"logs": "read"})
	st.CreateUserIn("logsonly", "acme", "logsonly-long-password", g.ID)
	a := auth.New(st, nil, false)
	box, _ := secretbox.New("k")
	fs := &fakeStore{}
	ts := httptest.NewServer(New(fs, a, slog.New(slog.NewTextHandler(io.Discard, nil))).WithAuth(a).WithDashboards(dashboards.New(f)).
		WithRegistry(registry.New(f, box)).WithBranding(branding.New(f)).Handler())
	t.Cleanup(ts.Close)
	return ts, fs
}

func TestFacetsAndFilters(t *testing.T) {
	ts, fs := settingsServer(t)
	ad, rd, lo := client(), client(), client()
	login(t, ad, ts, "admin", "admins-long-password")
	login(t, rd, ts, "reader", "readers-long-password")
	login(t, lo, ts, "logsonly", "logsonly-long-password")
	fs.facets = model.Facets{Services: []model.Facet{{Name: "checkout", Count: 10}, {Name: "web", Count: 5}}, Hosts: []model.Facet{{Name: "web1", Count: 15}}, Operations: []model.Facet{{Name: "GET /cart", Count: 7}}}
	_, b := do(rd, "GET", ts.URL+"/api/v1/facets?source=traces", "")
	var f model.Facets
	json.Unmarshal(b, &f)
	if len(f.Services) != 2 || f.Services[0].Name != "checkout" || f.Services[0].Count != 10 || len(f.Operations) != 1 || len(f.Hosts) != 1 {
		t.Fatalf("facets: %s", b)
	}
	// cached for a minute: a second identical call does not hit the database again
	do(rd, "GET", ts.URL+"/api/v1/facets?source=traces", "")
	if fs.facetCalls != 1 {
		t.Fatalf("facets must be cached, store called %d times", fs.facetCalls)
	}
	do(rd, "GET", ts.URL+"/api/v1/facets?source=traces&service=web", "")
	if fs.facetCalls != 2 || fs.lastFacet != "traces|web" {
		t.Fatalf("a different service is a different list: %d %s", fs.facetCalls, fs.lastFacet)
	}
	// permissions follow the signal
	if code(lo, "GET", ts.URL+"/api/v1/facets?source=logs", "") != 200 || code(lo, "GET", ts.URL+"/api/v1/facets?source=traces", "") != 403 {
		t.Fatal("a user who may only read logs gets the logs lists and not the traces lists")
	}
	if code(rd, "GET", ts.URL+"/api/v1/facets?source=metrics", "") != 400 || code(rd, "GET", ts.URL+"/api/v1/facets", "") != 400 {
		t.Fatal("unknown source")
	}
	if code(rd, "GET", ts.URL+"/api/v1/facets?source=logs&archive=1", "") != 403 {
		t.Fatal("archive needs the backups permission")
	}
	// host and operation filters reach the store
	do(rd, "GET", ts.URL+"/api/v1/logs?host=web1&service=nginx", "")
	if fs.lastLogQ.Host != "web1" || fs.lastLogQ.Service != "nginx" {
		t.Fatalf("log filters: %+v", fs.lastLogQ)
	}
	do(rd, "GET", ts.URL+"/api/v1/traces?host=web1&operation=GET+%2Fcart&service=web", "")
	if fs.lastTraceQ.Host != "web1" || fs.lastTraceQ.Operation != "GET /cart" || fs.lastTraceQ.Service != "web" {
		t.Fatalf("trace filters: %+v", fs.lastTraceQ)
	}
	_ = ad
}

func TestRenameAndRemoveHostAPI(t *testing.T) {
	ts, fs := settingsServer(t)
	ad, rd := client(), client()
	login(t, ad, ts, "admin", "admins-long-password")
	login(t, rd, ts, "reader", "readers-long-password")
	now := time.Now().Unix()
	fs.latest = []model.Latest{
		{Name: "lumen_agent_info", Attrs: map[string]string{"host": "b4d226bb6dfe", "version": "0.4"}, Value: 1, T: now - 3},
		{Name: "lumen_agent_info", Attrs: map[string]string{"host": "oldbox", "version": "0.4"}, Value: 1, T: now - 7200},
	}
	if code(ad, "PUT", ts.URL+"/api/v1/hosts/b4d226bb6dfe", `{"display_name":"Nextcloud KS","systemd":true,"containers":true}`) != 200 {
		t.Fatal("rename")
	}
	_, b := do(rd, "GET", ts.URL+"/api/v1/hosts", "")
	if !strings.Contains(string(b), `"display_name":"Nextcloud KS"`) || !strings.Contains(string(b), `"name":"b4d226bb6dfe"`) {
		t.Fatalf("both the real name and the display name are returned: %s", b)
	}
	if code(ad, "PUT", ts.URL+"/api/v1/hosts/b4d226bb6dfe", `{"display_name":"`+strings.Repeat("x", 80)+`"}`) != 400 {
		t.Fatal("a too long name")
	}
	if code(rd, "PUT", ts.URL+"/api/v1/hosts/b4d226bb6dfe", `{"display_name":"x"}`) != 403 || code(rd, "POST", ts.URL+"/api/v1/hosts/oldbox/remove", "") != 403 {
		t.Fatal("a read-only user cannot rename or remove")
	}
	// removal is blocked while an instance is checked by the host
	_, b = do(ad, "POST", ts.URL+"/api/v1/instances", `{"name":"ks","url":"https://ks.example.com","host":"oldbox"}`)
	var inst map[string]any
	json.Unmarshal(b, &inst)
	r, b := do(ad, "POST", ts.URL+"/api/v1/hosts/oldbox/remove", "")
	if r.StatusCode != 400 || !strings.Contains(string(b), "ks") {
		t.Fatalf("removal must be refused and name the instance: %d %s", r.StatusCode, b)
	}
	do(ad, "DELETE", ts.URL+"/api/v1/instances/"+inst["id"].(string), "")
	if code(ad, "POST", ts.URL+"/api/v1/hosts/oldbox/remove", "") != 200 {
		t.Fatal("remove")
	}
	_, b = do(rd, "GET", ts.URL+"/api/v1/hosts", "")
	if strings.Contains(string(b), "oldbox") || !strings.Contains(string(b), "b4d226bb6dfe") {
		t.Fatalf("the removed host is gone from the list at once, the other stays: %s", b)
	}
	_, b = do(rd, "GET", ts.URL+"/api/v1/status", "")
	if strings.Contains(string(b), "oldbox") {
		t.Fatalf("also gone from status: %s", b)
	}
	// the agent configuration is not affected by a rename
	if code(ad, "POST", ts.URL+"/api/v1/hosts/bad%20host/remove", "") != 400 {
		t.Fatal("invalid host name")
	}
}

func TestBrandingAPI(t *testing.T) {
	ts, _ := settingsServer(t)
	ad, rd := client(), client()
	login(t, ad, ts, "admin", "admins-long-password")
	login(t, rd, ts, "reader", "readers-long-password")
	// public, no login needed: the login page shows the logo
	resp, _ := http.Get(ts.URL + "/api/v1/branding")
	var p branding.Public
	json.NewDecoder(resp.Body).Decode(&p)
	if resp.StatusCode != 200 || p.HasLogo || p.Name != "" {
		t.Fatalf("default: %d %+v", resp.StatusCode, p)
	}
	if r, _ := http.Get(ts.URL + "/branding/logo"); r.StatusCode != 404 {
		t.Fatalf("no logo yet: %d", r.StatusCode)
	}
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	body := `{"name":"Tracexit","logo":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(png) + `"}`
	if code(rd, "PUT", ts.URL+"/api/v1/settings/branding", body) != 403 {
		t.Fatal("a read-only user must not change the logo")
	}
	if code(client(), "PUT", ts.URL+"/api/v1/settings/branding", body) != 401 {
		t.Fatal("anonymous must not change the logo")
	}
	r, b := do(ad, "PUT", ts.URL+"/api/v1/settings/branding", body)
	json.Unmarshal(b, &p)
	if r.StatusCode != 200 || p.Name != "Tracexit" || !p.HasLogo || p.Version == "" {
		t.Fatalf("upload: %d %s", r.StatusCode, b)
	}
	resp, _ = http.Get(ts.URL + "/branding/logo?v=" + p.Version)
	got, _ := io.ReadAll(resp.Body)
	h := resp.Header
	if resp.StatusCode != 200 || string(got) != string(png) || h.Get("Content-Type") != "image/png" || h.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(h.Get("Content-Security-Policy"), "sandbox") || !strings.Contains(h.Get("Cache-Control"), "max-age") {
		t.Fatalf("logo response: %d %v", resp.StatusCode, h)
	}
	// dangerous or wrong files are refused and change nothing
	for _, bad := range []string{
		`{"name":"x","logo":"data:image/svg+xml;base64,` + base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)) + `"}`,
		`{"name":"x","logo":"data:image/png;base64,` + base64.StdEncoding.EncodeToString([]byte("<html>")) + `"}`,
		`{"name":"x","logo":"https://evil.example/logo.png"}`,
		`{"name":"<img src=x onerror=alert(1)>"}`,
	} {
		if c := code(ad, "PUT", ts.URL+"/api/v1/settings/branding", bad); c != 400 {
			t.Errorf("%.80s must be 400, got %d", bad, c)
		}
	}
	resp, _ = http.Get(ts.URL + "/api/v1/branding")
	json.NewDecoder(resp.Body).Decode(&p)
	if p.Name != "Tracexit" || !p.HasLogo {
		t.Fatalf("failed uploads must change nothing: %+v", p)
	}
	// too large a request body is refused
	huge := `{"name":"x","logo":"data:image/png;base64,` + strings.Repeat("A", 2<<20) + `"}`
	if c := code(ad, "PUT", ts.URL+"/api/v1/settings/branding", huge); c != 400 {
		t.Fatalf("a huge body: %d", c)
	}
	// remove the logo, keep the name
	code(ad, "PUT", ts.URL+"/api/v1/settings/branding", `{"name":"Tracexit","remove_logo":true}`)
	if r, _ := http.Get(ts.URL + "/branding/logo"); r.StatusCode != 404 {
		t.Fatal("logo removed")
	}
	// the permission is part of group editing: the built-in User group has none, Admin has write
	_, b = do(ad, "GET", ts.URL+"/api/v1/groups", "")
	if !strings.Contains(string(b), `"settings"`) {
		t.Fatalf("the group editor must list the Settings area: %s", b)
	}
}
