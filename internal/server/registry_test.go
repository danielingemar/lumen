package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/dashboards"
	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/model"
	"github.com/danielingemar/lumen/internal/registry"
	"github.com/danielingemar/lumen/internal/secretbox"
)

func regServer(t *testing.T) (*httptest.Server, *auth.Auth, *fakeStore) {
	f, _ := docstore.OpenFile(t.TempDir())
	st, _ := auth.Open(f, 0)
	st.CreateUser("admin", "acme", "admins-long-password")
	st.CreateUserIn("reader", "acme", "readers-long-password", "user")
	st.CreateUser("other", "globex", "others-long-password")
	a := auth.New(st, nil, false)
	box, _ := secretbox.New("k")
	fs := &fakeStore{}
	ts := httptest.NewServer(New(fs, a, slog.New(slog.NewTextHandler(io.Discard, nil))).WithAuth(a).
		WithDashboards(dashboards.New(f)).WithRegistry(registry.New(f, box)).Handler())
	t.Cleanup(ts.Close)
	return ts, a, fs
}

func TestInstancesAPIPermissionsAndSecrets(t *testing.T) {
	ts, a, fs := regServer(t)
	ad, rd, ot := client(), client(), client()
	login(t, ad, ts, "admin", "admins-long-password")
	login(t, rd, ts, "reader", "readers-long-password")
	login(t, ot, ts, "other", "others-long-password")
	now := time.Now().Unix()
	fs.latestFor = "acme"
	fs.latest = []model.Latest{
		{Name: "lumen_agent_info", Attrs: map[string]string{"host": "web1", "version": "0.3", "os": "linux/amd64"}, Value: 1, T: now - 3},
		{Name: "nextcloud_up", Service: "ks", Attrs: map[string]string{"instance": "cloud.example.com", "host": "web1"}, Value: 0, T: now - 3},
	}
	r, b := do(ad, "POST", ts.URL+"/api/v1/instances", `{"name":"ks","url":"https://cloud.example.com","host":"web1","token":"TOPSECRET"}`)
	var out map[string]any
	json.Unmarshal(b, &out)
	if r.StatusCode != 200 || out["has_token"] != true || strings.Contains(string(b), "TOPSECRET") {
		t.Fatalf("create: %d %s (secrets must never be returned)", r.StatusCode, b)
	}
	id := out["id"].(string)
	_, b = do(rd, "GET", ts.URL+"/api/v1/instances", "")
	if strings.Contains(string(b), "TOPSECRET") || !strings.Contains(string(b), `"status":"down"`) || !strings.Contains(string(b), "cannot reach") {
		t.Fatalf("a reader sees the live status (down: reports 0), without secrets: %s", b)
	}
	if code(rd, "POST", ts.URL+"/api/v1/instances", `{"name":"x","url":"https://x.com","host":"web1"}`) != 403 ||
		code(rd, "PUT", ts.URL+"/api/v1/instances/"+id, `{"name":"x","url":"https://x.com","host":"web1"}`) != 403 ||
		code(rd, "DELETE", ts.URL+"/api/v1/instances/"+id, "") != 403 || code(rd, "PUT", ts.URL+"/api/v1/hosts/web1", `{"log_paths":["/x"]}`) != 403 {
		t.Fatal("a read-only user must not change instances or hosts")
	}
	if code(ad, "PUT", ts.URL+"/api/v1/instances/"+id, `{"name":"ks","url":"https://bad host","host":"web1"}`) != 400 {
		t.Fatal("invalid URL")
	}
	if code(ad, "PUT", ts.URL+"/api/v1/instances/"+id, `{"name":"ks","url":"https://cloud.example.org","host":"web1"}`) != 200 {
		t.Fatal("fix the URL")
	}
	_, b = do(ot, "GET", ts.URL+"/api/v1/instances", "")
	if strings.Contains(string(b), "ks") {
		t.Fatalf("other tenant sees instances: %s", b)
	}
	if code(ot, "PUT", ts.URL+"/api/v1/instances/"+id, `{"name":"ks","url":"https://evil.example.com","host":"web1"}`) != 404 || code(ot, "DELETE", ts.URL+"/api/v1/instances/"+id, "") != 404 {
		t.Fatal("other tenant must not change it")
	}
	// the agent fetches its config with a key: decrypted token, only its own instances
	plain, _, _ := a.Store.CreateKey("acme", "web1")
	get := func(c *http.Client, key, p string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", ts.URL+p, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		bb, _ := io.ReadAll(resp.Body)
		return resp, string(bb)
	}
	resp, body := get(http.DefaultClient, plain, "/api/v1/agent/config?host=web1")
	if resp.StatusCode != 200 || !strings.Contains(body, "TOPSECRET") || !strings.Contains(body, "cloud.example.org") || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("agent config: %d %s", resp.StatusCode, body)
	}
	if resp, body := get(http.DefaultClient, plain, "/api/v1/agent/config?host=other-host"); strings.Contains(body, "TOPSECRET") || resp.StatusCode != 200 {
		t.Fatalf("an agent must only get its own host's instances: %s", body)
	}
	if resp, _ := get(ad, "", "/api/v1/agent/config?host=web1"); resp.StatusCode != 403 {
		t.Fatalf("a browser session must not be able to read instance secrets: %d", resp.StatusCode)
	}
	if resp, _ := get(http.DefaultClient, "", "/api/v1/agent/config?host=web1"); resp.StatusCode != 401 {
		t.Fatalf("anonymous: %d", resp.StatusCode)
	}
	if resp, _ := get(http.DefaultClient, plain, "/api/v1/agent/config"); resp.StatusCode != 400 {
		t.Fatal("host is required")
	}
}

func TestHostsAndStatusAPI(t *testing.T) {
	ts, _, fs := regServer(t)
	ad, rd := client(), client()
	login(t, ad, ts, "admin", "admins-long-password")
	login(t, rd, ts, "reader", "readers-long-password")
	now := time.Now().Unix()
	fs.latest = []model.Latest{
		{Name: "lumen_agent_info", Attrs: map[string]string{"host": "web1", "version": "0.3", "os": "linux/amd64"}, Value: 1, T: now - 3},
		{Name: "system_service_up", Attrs: map[string]string{"host": "web1", "service": "nginx", "state": "running"}, Value: 1, T: now - 3},
		{Name: "system_service_up", Attrs: map[string]string{"host": "web1", "service": "postfix", "state": "failed"}, Value: 0, T: now - 3},
		{Name: "container_up", Attrs: map[string]string{"host": "web1", "container": "app", "state": "running"}, Value: 1, T: now - 3},
	}
	if c := code(ad, "PUT", ts.URL+"/api/v1/hosts/web1", `{"log_paths":["/var/log/nginx/*.log"," "],"docker_logs":true,"systemd":true,"containers":true,"watch_services":["nginx"]}`); c != 200 {
		t.Fatalf("put host: %d", c)
	}
	var st struct {
		Hosts, Services, Containers, Nextcloud struct{ Up, Down int }
		Down                                   []map[string]any
	}
	_, b := do(rd, "GET", ts.URL+"/api/v1/status", "")
	json.Unmarshal(b, &st)
	if st.Hosts.Up != 1 || st.Services.Up != 1 || st.Services.Down != 1 || st.Containers.Up != 1 || len(st.Down) != 1 || st.Down[0]["name"] != "postfix" {
		t.Fatalf("status: %s", b)
	}
	_, b = do(rd, "GET", ts.URL+"/api/v1/hosts", "")
	if !strings.Contains(string(b), `"/var/log/nginx/*.log"`) || !strings.Contains(string(b), `"status":"up"`) {
		t.Fatalf("hosts list shows config and status: %s", b)
	}
	_, b = do(rd, "GET", ts.URL+"/api/v1/hosts/web1", "")
	if !strings.Contains(string(b), `"postfix"`) || !strings.Contains(string(b), `"app"`) {
		t.Fatalf("host detail lists services and containers: %s", b)
	}
	// the status panel must stay cheap: repeated calls reuse one database query
	fs.latestErr = io.ErrUnexpectedEOF
	if c := code(rd, "GET", ts.URL+"/api/v1/status", ""); c != 200 {
		t.Fatalf("a second call within a few seconds must be served from the short cache, got %d", c)
	}
	// saving a host's settings is visible immediately (cache dropped)
	if c := code(ad, "PUT", ts.URL+"/api/v1/hosts/web1", `{"log_paths":[],"systemd":true,"containers":true}`); c != 200 {
		t.Fatal("put")
	}
	if c := code(rd, "GET", ts.URL+"/api/v1/status", ""); c != 500 {
		t.Fatalf("after a change the next call recomputes (and here the database is down): %d", c)
	}
}

func TestInstancesThatAreGoneCanBeRemoved(t *testing.T) {
	ts, _, fs := regServer(t)
	ad, rd, ot := client(), client(), client()
	login(t, ad, ts, "admin", "admins-long-password")
	login(t, rd, ts, "reader", "readers-long-password")
	login(t, ot, ts, "other", "others-long-password")
	now := time.Now().Unix()
	unm := func(name, key string, v float64, age int64) model.Latest {
		return model.Latest{Name: "nextcloud_up", Service: name, Attrs: map[string]string{"instance": key, "host": "web1"}, Value: v, T: now - age}
	}
	fs.latestFor = "acme"
	fs.latest = []model.Latest{
		{Name: "lumen_agent_info", Attrs: map[string]string{"host": "web1"}, Value: 1, T: now - 3},
		unm("legacy", "old.example.com", 0, 600), unm("unreach", "unreach.example.com", 0, 20), unm("alive", "alive.example.com", 1, 3), unm("stale", "stale.example.com", 1, 600), unm("ks", "ks.example.com", 1, 3),
	}
	names := func() string {
		_, b := do(ad, "GET", ts.URL+"/api/v1/instances", "")
		var out struct {
			Data []struct {
				Name    string
				Managed bool
			}
		}
		json.Unmarshal(b, &out)
		var n []string
		for _, i := range out.Data {
			m := "u"
			if i.Managed {
				m = "m"
			}
			n = append(n, i.Name+":"+m)
		}
		sort.Strings(n)
		return strings.Join(n, ",")
	}
	if got := names(); got != "alive:u,ks:u,legacy:u,stale:u,unreach:u" {
		t.Fatalf("%s", got)
	}
	rm := func(c *http.Client, key string) (int, string) {
		r, b := do(c, "POST", ts.URL+"/api/v1/instance-keys/"+key+"/remove", "")
		return r.StatusCode, string(b)
	}
	// who may
	if c, _ := rm(rd, "old.example.com"); c != 403 {
		t.Fatalf("a read-only user cannot remove: %d", c)
	}
	if c, _ := rm(ot, "old.example.com"); c != 404 {
		t.Fatalf("another tenant has no such instance, so it cannot remove ours: %d", c)
	}
	// what may be removed
	if c, b := rm(ad, "alive.example.com"); c != 400 || !strings.Contains(b, "is up and its agent still reports it") {
		t.Fatalf("an instance that is up is refused, with the reason: %d %s", c, b)
	}
	if c, b := rm(ad, "unreach.example.com"); c != 400 || !strings.Contains(b, "its agent still reports it") || !strings.Contains(b, "would only come back") {
		t.Fatalf("one whose agent keeps reporting that it cannot reach it would only come back: %d %s", c, b)
	}
	if c, _ := rm(ad, "nothing.example.com"); c != 404 {
		t.Fatalf("unknown: %d", c)
	}
	// a managed instance has its own Remove
	do(ad, "POST", ts.URL+"/api/v1/instances", `{"name":"mine","url":"https://mine.example.com","host":"web1"}`)
	if c, b := rm(ad, "mine.example.com"); c != 400 || !strings.Contains(b, "managed here") {
		t.Fatalf("%d %s", c, b)
	}
	// the ones that are gone
	if c, _ := rm(ad, "old.example.com"); c != 200 {
		t.Fatalf("%d", c)
	}
	if c, _ := rm(ad, "stale.example.com"); c != 200 {
		t.Fatalf("%d", c)
	}
	if got := names(); got != "alive:u,ks:u,mine:m,unreach:u" {
		t.Fatalf("removed ones are gone from the list at once: %s", got)
	}
	_, b := do(ad, "GET", ts.URL+"/api/v1/status", "")
	if !strings.Contains(string(b), `"nextcloud":{"up":1,"down":1}`) && strings.Contains(string(b), "legacy") {
		t.Fatalf("and from the status boxes: %s", b)
	}
	// it comes back only if its agent reports it again, well after the removal
	fs.latest[1] = unm("legacy", "old.example.com", 1, -5) // reports again after it was removed
	time.Sleep(10 * time.Millisecond)
	do(ad, "POST", ts.URL+"/api/v1/instances", `{"name":"tmp","url":"https://tmp.example.com","host":"web1"}`) // any write forgets the cached status
	if got := names(); !strings.Contains(got, "legacy:u") {
		t.Fatalf("an instance that reports again is listed again: %s", got)
	}
	// removing an instance that is managed here also hides what its agent still reports for a moment
	_, b = do(ad, "POST", ts.URL+"/api/v1/instances", `{"name":"ks","url":"https://ks.example.com","host":"web1"}`)
	var created map[string]any
	json.Unmarshal(b, &created)
	if got := names(); !strings.Contains(got, "ks:m") || strings.Contains(got, "ks:u") {
		t.Fatalf("%s", got)
	}
	if c := code(ad, "DELETE", ts.URL+"/api/v1/instances/"+created["id"].(string), ""); c != 200 {
		t.Fatal(c)
	}
	if got := names(); strings.Contains(got, "ks:") {
		t.Fatalf("a removed instance whose agent has just reported must not turn up as an unmanaged one: %s", got)
	}
	// and adding it again at the same address works
	if c := code(ad, "POST", ts.URL+"/api/v1/instances", `{"name":"ks","url":"https://ks.example.com","host":"web1"}`); c != 200 {
		t.Fatal(c)
	}
	if got := names(); !strings.Contains(got, "ks:m") {
		t.Fatalf("%s", got)
	}
}

func TestAnEmptyInstanceListIsAnEmptyArray(t *testing.T) {
	ts, _, _ := regServer(t)
	ad := client()
	login(t, ad, ts, "admin", "admins-long-password")
	_, b := do(ad, "GET", ts.URL+"/api/v1/instances", "")
	if !strings.Contains(string(b), `"data":[]`) {
		t.Fatalf("scripts that read the API expect an array, not null: %s", b)
	}
}
