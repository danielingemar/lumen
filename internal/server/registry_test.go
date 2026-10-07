package server

import (
	"encoding/json"
	"github.com/danielingemar/lumen/internal/buildinfo"
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

func TestOneClickAgentUpdate(t *testing.T) {
	ts, a, fs := regServer(t)
	ad, rd, ot := client(), client(), client()
	login(t, ad, ts, "admin", "admins-long-password")
	login(t, rd, ts, "reader", "readers-long-password")
	login(t, ot, ts, "other", "others-long-password")
	acmeKey, _, _ := a.Store.CreateKey("acme", "agent")
	globexKey, _, _ := a.Store.CreateKey("globex", "agent")
	old := buildinfo.Version
	defer func() { buildinfo.Version = old }()
	now := time.Now().Unix()
	info := func(host, version, self string) model.Latest {
		at := map[string]string{"host": host, "version": version, "os": "linux/amd64"}
		if self != "" {
			at["self_update"] = self
		}
		return model.Latest{Name: "lumen_agent_info", Attrs: at, Value: 1, T: now - 3}
	}
	fs.latestFor = "acme"
	fs.latest = []model.Latest{info("web1", "src-old", "systemd"), info("web2", "src-old", ""), info("web3", "src-new", "systemd"), info("web4", "src-old", "exec")}
	cfg := func(key, host, version string) map[string]any {
		req, _ := http.NewRequest("GET", ts.URL+"/api/v1/agent/config?host="+host+"&version="+version, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		return m
	}
	hosts := func() map[string]map[string]any {
		_, b := do(ad, "GET", ts.URL+"/api/v1/hosts", "")
		var out struct{ Data []map[string]any }
		json.Unmarshal(b, &out)
		m := map[string]map[string]any{}
		for _, h := range out.Data {
			m[h["name"].(string)] = h
		}
		return m
	}
	// a server without a version stamp does not know what to update to
	buildinfo.Version = "dev"
	if r, b := do(ad, "POST", ts.URL+"/api/v1/hosts/web1/update", ""); r.StatusCode != 400 || !strings.Contains(string(b), "without a version stamp") {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	buildinfo.Version = "src-new"
	// who may
	if c := code(rd, "POST", ts.URL+"/api/v1/hosts/web1/update", ""); c != 403 {
		t.Fatalf("read-only users cannot update agents: %d", c)
	}
	if c := code(ot, "POST", ts.URL+"/api/v1/hosts/web1/update", ""); c != 404 {
		t.Fatalf("another tenant has no such host: %d", c)
	}
	if c := code(ad, "POST", ts.URL+"/api/v1/hosts/nothing/update", ""); c != 404 {
		t.Fatal(c)
	}
	// what cannot be done, with the way out
	if r, b := do(ad, "POST", ts.URL+"/api/v1/hosts/web2/update", ""); r.StatusCode != 400 || !strings.Contains(string(b), "cannot update itself") || !strings.Contains(string(b), "--update") {
		t.Fatalf("an agent that cannot update itself is told what to do instead: %d %s", r.StatusCode, b)
	}
	if r, b := do(ad, "POST", ts.URL+"/api/v1/hosts/web3/update", ""); r.StatusCode != 400 || !strings.Contains(string(b), "already has the version") {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	// nothing is asked of an agent that has not been asked
	if c := cfg(acmeKey, "web1", "src-old"); c["update_to"] != nil {
		t.Fatalf("%v", c)
	}
	if hosts()["web1"]["update_requested"] != nil || hosts()["web1"]["self_update"] != "systemd" {
		t.Fatalf("the host list says how the agent updates and that nothing is asked: %v", hosts()["web1"])
	}
	// the click
	r, b := do(ad, "POST", ts.URL+"/api/v1/hosts/web1/update", "")
	if r.StatusCode != 200 || !strings.Contains(string(b), `"mode":"systemd"`) || !strings.Contains(string(b), `"target":"src-new"`) {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	if hosts()["web1"]["update_requested"] == nil {
		t.Fatal("the host list shows that an update was asked for")
	}
	if c := cfg(acmeKey, "web1", "src-old"); c["update_to"] != "src-new" {
		t.Fatalf("the agent is told which version to get: %v", c)
	}
	if c := cfg(acmeKey, "web2", "src-old"); c["update_to"] != nil {
		t.Fatal("only the host that was asked")
	}
	if c := cfg(globexKey, "web1", "src-old"); c["update_to"] != nil {
		t.Fatal("another tenant's host of the same name is not affected")
	}
	if c := cfg(acmeKey, "web1", "src-old"); c["revision"] == nil || c["revision"] == "" {
		t.Fatal("revision stays")
	}
	// the update must not change the configuration revision, or every agent would restart its collectors for nothing
	before := cfg(acmeKey, "web2", "src-old")["revision"]
	do(ad, "POST", ts.URL+"/api/v1/hosts/web4/update", "")
	if cfg(acmeKey, "web4", "src-old")["update_to"] != "src-new" {
		t.Fatal("exec agents too")
	}
	if cfg(acmeKey, "web2", "src-old")["revision"] != before {
		t.Fatal("asking for an update elsewhere does not touch other hosts' configuration")
	}
	// the agent has the new version: the request is closed
	if c := cfg(acmeKey, "web1", "src-new"); c["update_to"] != nil {
		t.Fatalf("%v", c)
	}
	if hosts()["web1"]["update_requested"] != nil {
		t.Fatal("closed once the agent reports the version asked for")
	}
	if c := cfg(acmeKey, "web1", "src-old"); c["update_to"] != nil {
		t.Fatal("and it stays closed: an agent that falls back is not pushed again until someone asks")
	}
	// update all: those that can, and a list of those that need the installer
	r, b = do(ad, "POST", ts.URL+"/api/v1/hosts-update-all", "")
	var all struct {
		Requested, Manual []string
		Target            string
	}
	json.Unmarshal(b, &all)
	sort.Strings(all.Requested)
	if r.StatusCode != 200 || strings.Join(all.Requested, ",") != "web1,web4" || strings.Join(all.Manual, ",") != "web2" || all.Target != "src-new" {
		t.Fatalf("web3 is current, web2 cannot do it itself: %d %s", r.StatusCode, b)
	}
	if c := code(rd, "POST", ts.URL+"/api/v1/hosts-update-all", ""); c != 403 {
		t.Fatal(c)
	}
	if hosts()["web3"]["update_requested"] != nil {
		t.Fatal("a current agent is not asked")
	}
}

func TestAgentsThatOnlyCheckInstancesAreNotHosts(t *testing.T) {
	ts, _, fs := regServer(t)
	ad := client()
	login(t, ad, ts, "admin", "admins-long-password")
	old := buildinfo.Version
	buildinfo.Version = "src-new"
	defer func() { buildinfo.Version = old }()
	now := time.Now().Unix()
	info := func(host, role, self string) model.Latest {
		at := map[string]string{"host": host, "version": "src-old", "os": "linux/amd64", "self_update": self}
		if role != "" {
			at["role"] = role
		}
		return model.Latest{Name: "lumen_agent_info", Attrs: at, Value: 1, T: now - 3}
	}
	fs.latestFor = "acme"
	fs.latest = []model.Latest{info("web1", "host", "systemd"), info("nc-checker", "checker", "exec"),
		{Name: "nextcloud_up", Service: "ks", Attrs: map[string]string{"instance": "cloud.example.com", "host": "nc-checker"}, Value: 1, T: now - 3}}
	_, b := do(ad, "GET", ts.URL+"/api/v1/hosts", "")
	var hl struct{ Data []struct{ Name, Role string } }
	json.Unmarshal(b, &hl)
	if len(hl.Data) != 1 || hl.Data[0].Name != "web1" {
		t.Fatalf("the host list has machines only: %s", b)
	}
	_, b = do(ad, "GET", ts.URL+"/api/v1/status", "")
	if !strings.Contains(string(b), `"hosts":{"up":1,"down":0}`) {
		t.Fatalf("and the count of hosts too: %s", b)
	}
	// the instance list carries the checkers, and they can be chosen as the one that checks an instance
	_, b = do(ad, "GET", ts.URL+"/api/v1/instances", "")
	var il struct {
		Hosts    []string
		Checkers []struct {
			Name       string
			SelfUpdate string `json:"self_update"`
			Role       string
		}
	}
	json.Unmarshal(b, &il)
	sort.Strings(il.Hosts)
	if len(il.Checkers) != 1 || il.Checkers[0].Name != "nc-checker" || il.Checkers[0].SelfUpdate != "exec" || il.Checkers[0].Role != "checker" || strings.Join(il.Hosts, ",") != "nc-checker,web1" {
		t.Fatalf("%s", b)
	}
	// it still has a page of its own (instances link to the one that checks them) and it can be updated like a host
	if r, b := do(ad, "GET", ts.URL+"/api/v1/hosts/nc-checker", ""); r.StatusCode != 200 || !strings.Contains(string(b), `"role":"checker"`) {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	if c := code(ad, "POST", ts.URL+"/api/v1/hosts/nc-checker/update", ""); c != 200 {
		t.Fatalf("a checker can be updated with one click: %d", c)
	}
	_, b = do(ad, "GET", ts.URL+"/api/v1/instances", "")
	if !strings.Contains(string(b), `"update_requested"`) {
		t.Fatalf("and the list says it is being updated: %s", b)
	}
	r, b := do(ad, "POST", ts.URL+"/api/v1/hosts-update-all", "")
	var all struct{ Requested []string }
	json.Unmarshal(b, &all)
	sort.Strings(all.Requested)
	if r.StatusCode != 200 || strings.Join(all.Requested, ",") != "nc-checker,web1" {
		t.Fatalf("update all covers both kinds of agent: %s", b)
	}
}

func TestHostGroupsApi(t *testing.T) {
	ts, _, fs := regServer(t)
	ad, rd, ot := client(), client(), client()
	login(t, ad, ts, "admin", "admins-long-password")
	login(t, rd, ts, "reader", "readers-long-password")
	login(t, ot, ts, "other", "others-long-password")
	post := func(c *http.Client, path, body string) (int, string) {
		r, b := do(c, "POST", ts.URL+path, body)
		return r.StatusCode, string(b)
	}
	groups := func(c *http.Client) map[string][]string {
		_, b := do(c, "GET", ts.URL+"/api/v1/host-groups", "")
		var out struct {
			Data []struct {
				Name  string
				Hosts []string
			}
		}
		json.Unmarshal(b, &out)
		m := map[string][]string{}
		for _, g := range out.Data {
			m[g.Name] = g.Hosts
		}
		return m
	}
	// who may
	if c, _ := post(rd, "/api/v1/host-groups/members", `{"group":"web","add":["web1"]}`); c != 403 {
		t.Fatalf("a read-only user cannot change groups: %d", c)
	}
	if r, _ := do(rd, "GET", ts.URL+"/api/v1/host-groups", ""); r.StatusCode != 200 {
		t.Fatal("but can see them")
	}
	// add hosts, several at a time, also hosts that have no settings yet
	if c, b := post(ad, "/api/v1/host-groups/members", `{"group":"  Web servers ","add":["web1","web2","web3"]}`); c != 200 || !strings.Contains(b, `"changed":3`) {
		t.Fatalf("%d %s", c, b)
	}
	post(ad, "/api/v1/host-groups/members", `{"group":"Customer ACME","add":["web1","web2"]}`)
	if g := groups(ad); strings.Join(g["Web servers"], ",") != "web1,web2,web3" || strings.Join(g["Customer ACME"], ",") != "web1,web2" {
		t.Fatalf("%v", g)
	}
	if len(groups(ot)) != 0 {
		t.Fatal("another tenant sees none of them")
	}
	if c, _ := post(ot, "/api/v1/host-groups/delete", `{"group":"Web servers"}`); c != 200 || len(groups(ad)) != 2 {
		t.Fatal("and cannot delete them")
	}
	// the host list carries the groups of each host
	_, b := do(ad, "GET", ts.URL+"/api/v1/hosts", "")
	if !strings.Contains(string(b), `"groups":["Customer ACME","Web servers"]`) {
		t.Fatalf("every host says which groups it is in: %s", b)
	}
	// refusals
	for name, body := range map[string]string{"bad name": `{"group":"a,b","add":["web1"]}`, "no name": `{"group":"","add":["web1"]}`, "bad host": `{"group":"x","add":["../etc"]}`} {
		if c, _ := post(ad, "/api/v1/host-groups/members", body); c != 400 {
			t.Errorf("%s: %d", name, c)
		}
	}
	// saving a host's settings keeps its groups when the request does not mention them (an old script, an older page) ...
	if r, _ := do(ad, "PUT", ts.URL+"/api/v1/hosts/web1", `{"display_name":"Primary web","log_paths":["/var/log/x.log"],"systemd":true,"containers":true}`); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	if g := groups(ad); strings.Join(g["Web servers"], ",") != "web1,web2,web3" {
		t.Fatalf("settings saved without a groups field must not drop the groups: %v", g)
	}
	// ... changes them when it does, and clears them with an empty list
	do(ad, "PUT", ts.URL+"/api/v1/hosts/web1", `{"display_name":"Primary web","systemd":true,"containers":true,"groups":["Only here"]}`)
	if g := groups(ad); strings.Join(g["Only here"], ",") != "web1" || strings.Join(g["Web servers"], ",") != "web2,web3" {
		t.Fatalf("%v", g)
	}
	do(ad, "PUT", ts.URL+"/api/v1/hosts/web1", `{"systemd":true,"containers":true,"groups":[]}`)
	if g := groups(ad); len(g["Only here"]) != 0 {
		t.Fatalf("%v", g)
	}
	if r, _ := do(ad, "PUT", ts.URL+"/api/v1/hosts/web1", `{"groups":"not a list"}`); r.StatusCode != 400 {
		t.Fatal("groups must be a list")
	}
	// rename and delete
	if c, b := post(ad, "/api/v1/host-groups/rename", `{"from":"web servers","to":"Frontend"}`); c != 200 || !strings.Contains(b, `"changed":2`) {
		t.Fatalf("a group is found without regard to case: %d %s", c, b)
	}
	if c, _ := post(ad, "/api/v1/host-groups/rename", `{"from":"Frontend","to":"bad,name"}`); c != 400 {
		t.Fatal("a bad new name")
	}
	if c, b := post(ad, "/api/v1/host-groups/delete", `{"group":"Frontend"}`); c != 200 || !strings.Contains(b, `"changed":2`) {
		t.Fatalf("%d %s", c, b)
	}
	// a group is a filter in traces, logs and series: the server turns its name into the hosts
	if g := groups(ad); strings.Join(g["Customer ACME"], ",") != "web2" {
		t.Fatalf("%v", g)
	}
	post(ad, "/api/v1/host-groups/members", `{"group":"Customer ACME","add":["web4"]}`)
	do(ad, "GET", ts.URL+"/api/v1/traces?group=customer+acme&service=checkout", "")
	do(ad, "GET", ts.URL+"/api/v1/logs?group=Customer+ACME&host=web2", "")
	do(ad, "GET", ts.URL+"/api/v1/series?source=logs&metric=count&group=Customer+ACME", "")
	if strings.Join(fs.lastTraceQ.Hosts, ",") != "web2,web4" || fs.lastTraceQ.Service != "checkout" {
		t.Fatalf("traces: %+v", fs.lastTraceQ)
	}
	if strings.Join(fs.lastLogQ.Hosts, ",") != "web2,web4" || fs.lastLogQ.Host != "web2" {
		t.Fatalf("logs: %+v", fs.lastLogQ)
	}
	if strings.Join(fs.lastSeries.Hosts, ",") != "web2,web4" {
		t.Fatalf("series: %+v", fs.lastSeries)
	}
	do(ad, "GET", ts.URL+"/api/v1/logs?group=No+such+group", "")
	if len(fs.lastLogQ.Hosts) != 1 || fs.lastLogQ.Hosts[0] != noHost {
		t.Fatalf("a group that has no hosts shows nothing, it does not show everything: %+v", fs.lastLogQ.Hosts)
	}
	do(ad, "GET", ts.URL+"/api/v1/logs", "")
	if len(fs.lastLogQ.Hosts) != 0 {
		t.Fatal("without a group there is no limit")
	}
	// another tenant's group of the same name is its own
	do(ot, "GET", ts.URL+"/api/v1/logs?group=Customer+ACME", "")
	if len(fs.lastLogQ.Hosts) != 1 || fs.lastLogQ.Hosts[0] != noHost {
		t.Fatalf("another tenant has no such group: %+v", fs.lastLogQ.Hosts)
	}
}
