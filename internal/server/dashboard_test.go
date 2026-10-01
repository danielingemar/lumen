package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/dashboards"
	"github.com/danielingemar/lumen/internal/docstore"
)

func dashServer(t *testing.T) (*httptest.Server, *auth.Auth) {
	f, _ := docstore.OpenFile(t.TempDir())
	st, _ := auth.Open(f, 0)
	st.CreateUser("alice", "acme", "alices-long-password")
	st.CreateUser("bob", "globex", "bobs-long-password")
	a := auth.New(st, nil, false)
	ts := httptest.NewServer(New(&fakeStore{}, a, slog.New(slog.NewTextHandler(io.Discard, nil))).WithAuth(a).WithDashboards(dashboards.New(f)).Handler())
	t.Cleanup(ts.Close)
	return ts, a
}

func do(c *http.Client, method, url, body string) (*http.Response, []byte) {
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		panic(err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestDashboardCRUDAndIsolation(t *testing.T) {
	ts, _ := dashServer(t)
	alice, bob := client(), client()
	login(t, alice, ts, "alice", "alices-long-password")
	login(t, bob, ts, "bob", "bobs-long-password")
	if resp, _ := do(http.DefaultClient, "GET", ts.URL+"/api/v1/dashboards", ""); resp.StatusCode != 401 {
		t.Fatal("dashboards need authentication")
	}
	resp, b := do(alice, "POST", ts.URL+"/api/v1/dashboards", `{"name":"Hosts","description":"d","body":{"panels":[{"id":"p1","title":"CPU"}]}}`)
	var d dashboards.Dashboard
	json.Unmarshal(b, &d)
	if resp.StatusCode != 200 || d.ID == "" || d.Tenant != "acme" || d.Version == "" || d.CreatedBy != "alice" {
		t.Fatalf("create: %d %s", resp.StatusCode, b)
	}
	// listing has no body; get has it
	_, b = do(alice, "GET", ts.URL+"/api/v1/dashboards", "")
	if !strings.Contains(string(b), `"Hosts"`) || strings.Contains(string(b), `"panels"`) {
		t.Fatalf("list: %s", b)
	}
	_, b = do(alice, "GET", ts.URL+"/api/v1/dashboards/"+d.ID, "")
	if !strings.Contains(string(b), `"CPU"`) {
		t.Fatalf("get: %s", b)
	}
	// another tenant sees nothing and cannot read, change or delete it
	_, b = do(bob, "GET", ts.URL+"/api/v1/dashboards", "")
	if !strings.Contains(string(b), `"data":[]`) {
		t.Fatalf("bob must not see alice's dashboards: %s", b)
	}
	for _, m := range []string{"GET", "PUT", "DELETE"} {
		if resp, _ := do(bob, m, ts.URL+"/api/v1/dashboards/"+d.ID, `{"name":"x","body":{"panels":[]}}`); resp.StatusCode != 404 {
			t.Fatalf("bob %s on alice's dashboard must be 404, got %d", m, resp.StatusCode)
		}
	}
	// optimistic concurrency: a stale version is refused
	resp, b = do(alice, "PUT", ts.URL+"/api/v1/dashboards/"+d.ID, `{"name":"Hosts v2","body":{"panels":[]},"version":"`+d.Version+`"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("update: %d %s", resp.StatusCode, b)
	}
	if resp, _ := do(alice, "PUT", ts.URL+"/api/v1/dashboards/"+d.ID, `{"name":"Hosts v3","body":{"panels":[]},"version":"`+d.Version+`"}`); resp.StatusCode != 409 {
		t.Fatalf("stale update must be 409, got %d", resp.StatusCode)
	}
	// validation
	for _, bad := range []string{`{"name":"","body":{"panels":[]}}`, `{"name":"x","body":"not an object"}`, `{"name":"x","body":{"panels":` + "[" + strings.Repeat("{},", 60) + "{}]}}"} {
		if resp, _ := do(alice, "POST", ts.URL+"/api/v1/dashboards", bad); resp.StatusCode != 400 {
			t.Fatalf("invalid dashboard %.40s must be 400, got %d", bad, resp.StatusCode)
		}
	}
	form, _ := http.NewRequest("POST", ts.URL+"/api/v1/dashboards", strings.NewReader("name=x"))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded") // what a cross-site HTML form would send
	if resp, _ := alice.Do(form); resp.StatusCode != 415 {
		t.Fatalf("non-JSON must be refused, got %d", resp.StatusCode)
	}
	if resp, _ := do(alice, "DELETE", ts.URL+"/api/v1/dashboards/"+d.ID, ""); resp.StatusCode != 200 {
		t.Fatal("owner delete failed")
	}
	if resp, _ := do(alice, "GET", ts.URL+"/api/v1/dashboards/"+d.ID, ""); resp.StatusCode != 404 {
		t.Fatal("deleted dashboard must be gone")
	}
}

func TestDashboardsViaAPIKeyAndKeySession(t *testing.T) {
	ts, a := dashServer(t)
	plain, _, _ := a.Store.CreateKey("acme", "ci")
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/dashboards", strings.NewReader(`{"name":"From key","body":{"panels":[]}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+plain)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("an API key may manage dashboards for scripting: %v %v", resp, err)
	}
	c := client()
	post(c, ts.URL+"/api/v1/login", `{"api_key":"`+plain+`"}`)
	if r, b := do(c, "GET", ts.URL+"/api/v1/dashboards", ""); r.StatusCode != 200 || !strings.Contains(string(b), "From key") {
		t.Fatalf("a key session sees the tenant's dashboards: %d %s", r.StatusCode, b)
	}
}
