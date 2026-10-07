package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/audit"
	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/dashboards"
	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/license"
	"github.com/danielingemar/lumen/internal/metering"
	"github.com/danielingemar/lumen/internal/perm"
	"github.com/danielingemar/lumen/internal/tenants"
)

type purgeFake struct{ purged []string }

func (p *purgeFake) PurgeTenant(_ context.Context, t string) error {
	p.purged = append(p.purged, t)
	return nil
}
func (p *purgeFake) TenantRows(context.Context, string) (int64, error) { return 0, nil }

type tenEnv struct {
	ts    *httptest.Server
	fs    *fakeStore
	mgr   *license.Manager
	priv  ed25519.PrivateKey
	ten   *Tenancy
	store *auth.Store
	pg    *purgeFake
	keys  map[string]string // tenant -> API key
}

const pw = "a-long-password-123"

func tenServer(t *testing.T) *tenEnv {
	f, _ := docstore.OpenFile(t.TempDir())
	st, _ := auth.Open(f, 0)
	st.CreateUserIn("ops", perm.OperatorTenant, pw, "admin")
	st.CreateUserIn("acme-admin", "acme", pw, "admin")
	st.CreateUserIn("acme-user", "acme", pw, "user")
	st.CreateUserIn("globex-admin", "globex", pw, "admin")
	a := auth.New(st, nil, false)
	pub, priv, _ := license.NewKeyPair()
	mgr := license.NewManager(f, license.Keys{"main": pub}, "")
	fs := &fakeStore{}
	pg := &purgeFake{}
	tsvc := tenants.New(f)
	tsvc.Ensure([]string{perm.OperatorTenant, "acme", "globex"})
	ten := &Tenancy{Tenants: tsvc, Meter: metering.New(f), Limiter: metering.NewLimiter(), Audit: audit.New(f), Off: &tenants.Offboarder{Svc: tsvc, Purger: pg, BackupDir: t.TempDir()}}
	srv := New(fs, a, slog.New(slog.NewTextHandler(io.Discard, nil))).WithAuth(a).WithDashboards(dashboards.New(f)).WithLicense(mgr).WithTenancy(ten)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	e := &tenEnv{ts: ts, fs: fs, mgr: mgr, priv: priv, ten: ten, store: st, pg: pg, keys: map[string]string{}}
	for _, tn := range []string{"acme", "globex"} {
		k, _, _ := st.CreateKey(tn, "agent")
		e.keys[tn] = k
	}
	return e
}

func (e *tenEnv) licence(t *testing.T, editions ...string) {
	b, err := license.Sign(e.priv, "main", license.Payload{ID: "L", Customer: "Provider AB", Editions: editions, Issued: time.Now(), Expires: time.Now().AddDate(1, 0, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.mgr.Set(b, "test"); err != nil {
		t.Fatal(err)
	}
}

func (e *tenEnv) as(t *testing.T, user string) *http.Client {
	c := client()
	login(t, c, e.ts, user, pw)
	return c
}

func (e *tenEnv) ingest(tenant, path, body string) *http.Response {
	req, _ := http.NewRequest("POST", e.ts.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.keys[tenant])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func logsBody(n int) string {
	recs := make([]string, n)
	for i := range recs {
		recs[i] = `{"timeUnixNano":"1700000000000000000","severityText":"INFO","body":{"stringValue":"hello"}}`
	}
	return `{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"web"}}]},"scopeLogs":[{"logRecords":[` + strings.Join(recs, ",") + `]}]}]}`
}

func metricsBody(host string) string {
	return `{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"host"}},{"key":"host.name","value":{"stringValue":"` + host + `"}}]},"scopeMetrics":[{"metrics":[{"name":"system.cpu.utilization","gauge":{"dataPoints":[{"timeUnixNano":"1700000000000000000","asDouble":0.5}]}}]}]}]}`
}

func jsonOf(b []byte) map[string]any {
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func TestOperatorConsoleNeedsTheLicenceAndThePermission(t *testing.T) {
	e := tenServer(t)
	ops, boss, user := e.as(t, "ops"), e.as(t, "acme-admin"), e.as(t, "acme-user")
	// no licence: the console is closed and says why; nothing else changes
	r, b := do(ops, "GET", e.ts.URL+"/api/v1/operator/tenants", "")
	if r.StatusCode != 403 || !strings.Contains(string(b), "Operator add-on") {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	e.licence(t, license.Enterprise) // Enterprise is not the Operator add-on
	if code(ops, "GET", e.ts.URL+"/api/v1/operator/tenants", "") != 403 {
		t.Fatal("an Enterprise licence does not open the Operator console")
	}
	e.licence(t, license.Operator)
	r, b = do(ops, "GET", e.ts.URL+"/api/v1/operator/tenants", "")
	var list struct{ Data []struct{ ID, Status string } }
	json.Unmarshal(b, &list)
	if r.StatusCode != 200 || len(list.Data) != 3 || list.Data[0].ID != "operator" {
		t.Fatalf("with the licence: the operator tenant first, then the others: %d %s", r.StatusCode, b)
	}
	// a customer's administrator and user never get in, licence or not
	for name, c := range map[string]*http.Client{"customer admin": boss, "customer user": user} {
		for _, x := range []struct{ m, p, b string }{
			{"GET", "/api/v1/operator/tenants", ""}, {"POST", "/api/v1/operator/tenants", `{"id":"evil","name":"x","admin_user":"evil"}`},
			{"GET", "/api/v1/operator/tenants/globex", ""}, {"PUT", "/api/v1/operator/tenants/globex", `{"name":"mine"}`},
			{"POST", "/api/v1/operator/tenants/globex/suspend", `{"reason":"x"}`}, {"POST", "/api/v1/operator/tenants/globex/enter", `{}`},
			{"POST", "/api/v1/operator/tenants/globex/offboard", `{"confirm":"globex"}`}, {"GET", "/api/v1/operator/tenants/globex/export", ""},
			{"GET", "/api/v1/operator/usage", ""}, {"GET", "/api/v1/operator/audit", ""},
		} {
			if c := code(c, x.m, e.ts.URL+x.p, x.b); c != 403 {
				t.Errorf("%s must not %s %s: %d", name, x.m, x.p, c)
			}
		}
	}
	// the group editor of a customer does not even list the console, and cannot be made to grant it
	_, b = do(boss, "GET", e.ts.URL+"/api/v1/users", "")
	if strings.Contains(string(b), "operator") {
		t.Fatalf("a customer's user list does not claim that its administrators have the operator console: %s", b)
	}
	_, b = do(boss, "GET", e.ts.URL+"/api/v1/groups", "")
	if strings.Contains(string(b), `"operator":"write"`) || strings.Contains(string(b), "write: alerts, backups, dashboards, hosts, keys, notifications, operator") {
		t.Fatalf("nor does the group list: %s", b)
	}
	if strings.Contains(string(b), `"id":"operator"`) {
		t.Fatalf("a customer's group editor lists the operator area: %s", b)
	}
	_, b = do(ops, "GET", e.ts.URL+"/api/v1/groups", "")
	if !strings.Contains(string(b), `"id":"operator"`) {
		t.Fatalf("the operator tenant's does: %s", b)
	}
	if c := code(boss, "POST", e.ts.URL+"/api/v1/groups", `{"name":"Sneaky","perms":{"operator":"write"}}`); c != 400 {
		t.Fatalf("a customer cannot create a group with the console: %d", c)
	}
}

func TestCreateSuspendResumeAndTheLimitsOfSuspension(t *testing.T) {
	e := tenServer(t)
	e.licence(t, license.Operator)
	ops := e.as(t, "ops")
	// create: the tenant, its first administrator, and a password that is shown once
	r, b := do(ops, "POST", e.ts.URL+"/api/v1/operator/tenants", `{"id":"initech","name":"Initech AB","contact":"it@initech.example","plan":"pro","admin_user":"bob"}`)
	out := jsonOf(b)
	pass, _ := out["admin"].(map[string]any)["password"].(string)
	if r.StatusCode != 200 || pass == "" || out["tenant"].(map[string]any)["support_access"] != "ask" || out["tenant"].(map[string]any)["status"] != "active" {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	bob := client()
	if c := post(bob, e.ts.URL+"/api/v1/login", `{"username":"bob","password":"`+pass+`"}`).StatusCode; c != 200 {
		t.Fatalf("the new administrator signs in with the generated password: %d", c)
	}
	for name, body := range map[string]string{
		"duplicate id": `{"id":"initech","name":"x","admin_user":"carol"}`, "reserved id": `{"id":"operator","name":"x","admin_user":"carol"}`,
		"bad id": `{"id":"Bad Id","name":"x","admin_user":"carol"}`, "no admin": `{"id":"newco","name":"x"}`, "no name": `{"id":"newco","admin_user":"carol"}`,
		"user name taken": `{"id":"newco","name":"x","admin_user":"bob"}`, "negative limit": `{"id":"newco","name":"x","admin_user":"carol","quotas":{"users":-1}}`,
	} {
		if c := code(ops, "POST", e.ts.URL+"/api/v1/operator/tenants", body); c != 400 {
			t.Errorf("%s must be refused: %d", name, c)
		}
	}
	if _, ok := e.ten.Tenants.Get("newco"); ok {
		t.Fatal("a tenant whose first administrator could not be made is not left behind")
	}
	// the detail
	_, b = do(ops, "GET", e.ts.URL+"/api/v1/operator/tenants/initech", "")
	d := jsonOf(b)
	if d["tenant"].(map[string]any)["name"] != "Initech AB" || len(d["users"].([]any)) != 1 || d["users"].([]any)[0].(map[string]any)["name"] != "bob" || strings.Contains(string(b), `"hash"`) {
		t.Fatalf("users are listed without password hashes: %s", b)
	}
	// suspend: the session, the sign-in and incoming data stop; other tenants are not affected
	initechKey, _, _ := e.store.CreateKey("initech", "agent")
	e.keys["initech"] = initechKey
	if e.ingest("initech", "/v1/logs", logsBody(2)).StatusCode != 200 {
		t.Fatal("before: ingest works")
	}
	if c := code(ops, "POST", e.ts.URL+"/api/v1/operator/tenants/initech/suspend", `{"reason":"unpaid invoice"}`); c != 200 {
		t.Fatal(c)
	}
	if r, b := do(bob, "GET", e.ts.URL+"/api/v1/me", ""); r.StatusCode != 403 || !strings.Contains(string(b), "suspended (unpaid invoice)") {
		t.Fatalf("an open session stops at once, with the reason: %d %s", r.StatusCode, b)
	}
	if r := post(client(), e.ts.URL+"/api/v1/login", `{"username":"bob","password":"`+pass+`"}`); r.StatusCode != 403 {
		t.Fatalf("signing in is refused: %d", r.StatusCode)
	}
	if r := post(client(), e.ts.URL+"/api/v1/login", `{"api_key":"`+initechKey+`"}`); r.StatusCode != 403 {
		t.Fatalf("also with a key: %d", r.StatusCode)
	}
	stored := len(e.fs.logs)
	if r := e.ingest("initech", "/v1/logs", logsBody(2)); r.StatusCode != 403 || len(e.fs.logs) != stored {
		t.Fatalf("data from a suspended tenant is refused and not stored: %d", r.StatusCode)
	}
	if e.ingest("acme", "/v1/logs", logsBody(2)).StatusCode != 200 {
		t.Fatal("another tenant is not affected")
	}
	// "drop": answered as accepted, thrown away
	do(ops, "PUT", e.ts.URL+"/api/v1/operator/tenants/initech", `{"name":"Initech AB","suspend_ingest":"drop"}`)
	stored = len(e.fs.logs)
	if r := e.ingest("initech", "/v1/logs", logsBody(2)); r.StatusCode != 200 || len(e.fs.logs) != stored {
		t.Fatalf("with 'drop' the agents see success but nothing is kept: status %d, rows %d (was %d)", r.StatusCode, len(e.fs.logs), stored)
	}
	// the operator can still go in to help, and the operator tenant cannot be suspended
	if c := code(ops, "POST", e.ts.URL+"/api/v1/operator/tenants/operator/suspend", `{"reason":"x"}`); c != 400 {
		t.Fatalf("nobody can lock the operators out: %d", c)
	}
	// resume
	if code(ops, "POST", e.ts.URL+"/api/v1/operator/tenants/initech/resume", ``) != 200 {
		t.Fatal("resume")
	}
	if e.ingest("initech", "/v1/logs", logsBody(2)).StatusCode != 200 || code(bob, "GET", e.ts.URL+"/api/v1/me", "") != 200 {
		t.Fatal("after resuming everything works again at once")
	}
	// without the Operator licence nothing is enforced: suspension is part of the add-on, and a lapsed licence must not cut customers off
	do(ops, "POST", e.ts.URL+"/api/v1/operator/tenants/initech/suspend", `{"reason":"again"}`)
	e.mgr.Remove()
	if e.ingest("initech", "/v1/logs", logsBody(2)).StatusCode != 200 || code(bob, "GET", e.ts.URL+"/api/v1/me", "") != 200 {
		t.Fatal("when the Operator licence ends, suspensions are not enforced (customers are never cut off by the operator's licence)")
	}
	// the audit log has it all
	e.licence(t, license.Operator)
	_, b = do(ops, "GET", e.ts.URL+"/api/v1/operator/audit?tenant=initech", "")
	for _, want := range []string{"tenant.create", "tenant.suspend", "tenant.update", "tenant.resume", "unpaid invoice"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the audit log lacks %q: %s", want, b)
		}
	}
}

func TestLimits(t *testing.T) {
	e := tenServer(t)
	e.licence(t, license.Operator)
	ops, boss := e.as(t, "ops"), e.as(t, "acme-admin")
	setQuota := func(tenant, q string) {
		if c := code(ops, "PUT", e.ts.URL+"/api/v1/operator/tenants/"+tenant, `{"name":"`+tenant+`","quotas":`+q+`}`); c != 200 {
			t.Fatalf("%d", c)
		}
	}
	// soft limits only warn
	setQuota("acme", `{"users":1,"keys":1}`)
	if c := code(boss, "POST", e.ts.URL+"/api/v1/users", `{"name":"soft-1","password":"`+pw+`","group":"user"}`); c != 200 {
		t.Fatalf("a soft limit never refuses: %d", c)
	}
	_, b := do(ops, "GET", e.ts.URL+"/api/v1/operator/tenants/acme", "")
	if w, _ := jsonOf(b)["warnings"].([]any); len(w) < 2 || !strings.Contains(fmt.Sprint(w), "users are in use, but the limit is 1") {
		t.Fatalf("it warns: %s", b)
	}
	// hard limits refuse, with a message a customer can act on
	setQuota("acme", `{"users":4,"keys":2,"groups":1,"dashboards":1,"hard":true}`)
	if c := code(boss, "POST", e.ts.URL+"/api/v1/users", `{"name":"hard-1","password":"`+pw+`","group":"user"}`); c != 200 {
		t.Fatalf("acme has 3 users (acme-admin, acme-user, soft-1) and the limit is 4: the fourth is allowed: %d", c)
	}
	r, b := do(boss, "POST", e.ts.URL+"/api/v1/users", `{"name":"hard-2","password":"`+pw+`","group":"user"}`)
	if r.StatusCode != 403 || !strings.Contains(string(b), "Limit reached: your plan allows 4 users") || !strings.Contains(string(b), "Ask your provider") {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	if c := code(boss, "POST", e.ts.URL+"/api/v1/keys", `{"name":"k2"}`); c != 200 {
		t.Fatalf("acme has 1 key from the setup and the limit is 2: one more is allowed: %d", c)
	}
	if c := code(boss, "POST", e.ts.URL+"/api/v1/keys", `{"name":"k3"}`); c != 403 {
		t.Fatalf("keys: %d", c)
	}
	if c := code(boss, "POST", e.ts.URL+"/api/v1/groups", `{"name":"G1","perms":{"users":"read"}}`); c != 200 {
		t.Fatalf("the first group is within the limit: %d", c)
	}
	if c := code(boss, "POST", e.ts.URL+"/api/v1/groups", `{"name":"G2","perms":{"users":"read"}}`); c != 403 {
		t.Fatalf("groups: %d", c)
	}
	dash := `{"name":"D","description":"","panels":[]}`
	code(boss, "POST", e.ts.URL+"/api/v1/dashboards", dash)
	if c := code(boss, "POST", e.ts.URL+"/api/v1/dashboards", strings.Replace(dash, `"D"`, `"D2"`, 1)); c != 403 {
		t.Fatalf("dashboards: %d", c)
	}
	if c := code(e.as(t, "globex-admin"), "POST", e.ts.URL+"/api/v1/users", `{"name":"gx2","password":"`+pw+`","group":"user"}`); c != 200 {
		t.Fatalf("another tenant has its own limits: %d", c)
	}
	// an operator inside the tenant is not stopped by its customer's limits (they are the customer's plan, not the operator's)
	e.ten.Tenants.SetSupportAccess("acme", tenants.SupportAllow)
	do(ops, "POST", e.ts.URL+"/api/v1/operator/tenants/acme/enter", `{"write":true}`)
	if c := code(ops, "POST", e.ts.URL+"/api/v1/users", `{"name":"by-operator","password":"`+pw+`","group":"user"}`); c != 200 {
		t.Fatalf("an operator inside a tenant can fix things even when the plan is full: %d", c)
	}
	code(ops, "POST", e.ts.URL+"/api/v1/operator/leave", ``)

	// incoming data: rate
	setQuota("acme", `{"ingest_per_sec":5,"hard":true}`)
	if r := e.ingest("acme", "/v1/logs", logsBody(40)); r.StatusCode != 200 {
		t.Fatalf("a burst of ten seconds' worth is fine: %d", r.StatusCode)
	}
	r = e.ingest("acme", "/v1/logs", logsBody(40))
	ra, _ := strconv.Atoi(r.Header.Get("Retry-After"))
	if r.StatusCode != 429 || ra < 1 || ra > 60 {
		t.Fatalf("over the rate: 429 with Retry-After so that exporters wait and retry: %d %q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	if e.ingest("globex", "/v1/logs", logsBody(400)).StatusCode != 200 {
		t.Fatal("one tenant's rate limit does not touch another")
	}
	// daily volume
	base := e.ten.Meter.BytesToday("acme") // what has been sent today already
	setQuota("acme", fmt.Sprintf(`{"ingest_bytes_per_day":%d,"hard":true}`, base+1500))
	ok := 0
	var last *http.Response
	for i := 0; i < 20; i++ {
		last = e.ingest("acme", "/v1/logs", logsBody(2))
		if last.StatusCode != 200 {
			break
		}
		ok++
	}
	ra, _ = strconv.Atoi(last.Header.Get("Retry-After"))
	if last.StatusCode != 429 || ok < 1 || ra < 1 || ra > 3600 {
		t.Fatalf("the daily volume runs out after %d requests: %d, retry after %q", ok, last.StatusCode, last.Header.Get("Retry-After"))
	}
	_, b = do(boss, "GET", e.ts.URL+"/api/v1/usage", "")
	u := jsonOf(b)
	if w := fmt.Sprint(u["warnings"]); !strings.Contains(w, "daily") && !strings.Contains(w, "data volume") {
		t.Fatalf("the customer sees the same in their own usage page: %s", b)
	}
	// hosts
	setQuota("acme", `{"hosts":2,"hard":true}`)
	stored := e.fs.metrics
	for _, h := range []string{"web1", "web2"} {
		if e.ingest("acme", "/v1/metrics", metricsBody(h)).StatusCode != 200 {
			t.Fatalf("host %s is within the limit", h)
		}
	}
	r = e.ingest("acme", "/v1/metrics", metricsBody("web3"))
	if r.StatusCode != 403 || e.fs.metrics != stored+2 {
		t.Fatalf("a third host is refused and not stored: %d", r.StatusCode)
	}
	if e.ingest("acme", "/v1/metrics", metricsBody("web1")).StatusCode != 200 {
		t.Fatal("a known host is never turned away")
	}
	setQuota("acme", `{"hosts":3,"hard":true}`)
	if e.ingest("acme", "/v1/metrics", metricsBody("web3")).StatusCode != 200 {
		t.Fatal("after the limit is raised the new host is let in")
	}
}

func TestMeteringAndUsage(t *testing.T) {
	e := tenServer(t)
	e.licence(t, license.Operator)
	ops, boss, gx := e.as(t, "ops"), e.as(t, "acme-admin"), e.as(t, "globex-admin")
	e.ingest("acme", "/v1/logs", logsBody(10))
	e.ingest("acme", "/v1/logs", logsBody(5))
	e.ingest("acme", "/v1/metrics", metricsBody("web1"))
	e.ingest("globex", "/v1/logs", logsBody(3))
	bad := e.ingest("acme", "/v1/logs", `{not json`)
	if bad.StatusCode != 400 {
		t.Fatal(bad.StatusCode)
	}
	e.ten.Meter.Flush()
	today := time.Now().UTC().Format("2006-01-02")
	r, b := do(ops, "GET", e.ts.URL+"/api/v1/operator/usage?tenant=acme", "")
	var us struct{ Data []metering.Day }
	json.Unmarshal(b, &us)
	if r.StatusCode != 200 || len(us.Data) != 1 || us.Data[0].Logs.Items != 15 || us.Data[0].Metrics.Items != 1 || us.Data[0].Day != today || us.Data[0].Bytes() <= 0 || len(us.Data[0].Hosts) != 1 {
		t.Fatalf("only accepted data is counted (not the malformed request): %d %s", r.StatusCode, b)
	}
	r, b = do(ops, "GET", e.ts.URL+"/api/v1/operator/usage?format=csv", "")
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/csv") || !strings.Contains(r.Header.Get("Content-Disposition"), "attachment") || !strings.Contains(string(b), "acme,"+today+",16,") || !strings.Contains(string(b), "globex,"+today+",3,") {
		t.Fatalf("the invoice basis as a spreadsheet: %d %s", r.StatusCode, b)
	}
	for _, q := range []string{"from=yesterday", "to=2026-13-45x", "from=2026-10-10&to=2026-10-01"} {
		if c := code(ops, "GET", e.ts.URL+"/api/v1/operator/usage?"+q, ""); c != 400 {
			t.Errorf("%s: %d", q, c)
		}
	}
	// a tenant sees its own use and nobody else's
	_, b = do(boss, "GET", e.ts.URL+"/api/v1/usage", "")
	own := jsonOf(b)
	if own["usage"].(map[string]any)["items"].(float64) != 16 || strings.Contains(string(b), "globex") || own["enforced"] != true {
		t.Fatalf("%s", b)
	}
	_, b = do(gx, "GET", e.ts.URL+"/api/v1/usage", "")
	if jsonOf(b)["usage"].(map[string]any)["items"].(float64) != 3 {
		t.Fatalf("%s", b)
	}
	if code(e.as(t, "acme-user"), "GET", e.ts.URL+"/api/v1/usage", "") != 403 {
		t.Fatal("a plain user does not see the account's usage")
	}
	// it is counted even without the Operator licence, so that the numbers exist from day one
	e.mgr.Remove()
	e.ingest("acme", "/v1/logs", logsBody(4))
	if e.ten.Meter.Today("acme").Logs.Items != 19 {
		t.Fatal("metering does not depend on the licence")
	}
}

func TestSupportAccessNeedsConsentAndIsOnRecord(t *testing.T) {
	e := tenServer(t)
	e.licence(t, license.Operator)
	ops := e.as(t, "ops")
	do(ops, "POST", e.ts.URL+"/api/v1/operator/tenants", `{"id":"initech","name":"Initech","admin_user":"bob","admin_password":"`+pw+`"}`)
	bob := e.as(t, "bob")
	// the default is "ask": nobody gets in until the customer says so
	r, b := do(ops, "POST", e.ts.URL+"/api/v1/operator/tenants/initech/enter", `{}`)
	if r.StatusCode != 400 || !strings.Contains(string(b), "granted access") {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	// the customer grants it for two hours
	_, b = do(bob, "GET", e.ts.URL+"/api/v1/support-access", "")
	if jsonOf(b)["mode"] != "ask" || jsonOf(b)["grant"] != nil {
		t.Fatalf("%s", b)
	}
	if c := code(bob, "POST", e.ts.URL+"/api/v1/support-access/grant", `{"minutes":1}`); c != 400 {
		t.Fatalf("a grant is at least 5 minutes: %d", c)
	}
	if c := code(bob, "POST", e.ts.URL+"/api/v1/support-access/grant", `{"minutes":120}`); c != 200 {
		t.Fatal(c)
	}
	// the operator goes in, read-only
	r, b = do(ops, "POST", e.ts.URL+"/api/v1/operator/tenants/initech/enter", `{"minutes":30}`)
	if r.StatusCode != 200 || jsonOf(b)["write"] != false {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	_, b = do(ops, "GET", e.ts.URL+"/api/v1/me", "")
	me := jsonOf(b)
	act, _ := me["acting"].(map[string]any)
	if me["tenant"] != "initech" || act == nil || act["operator"] != "ops" || act["write"] != false || act["until"] == "" {
		t.Fatalf("the interface is told who is inside which tenant: %s", b)
	}
	if code(ops, "GET", e.ts.URL+"/api/v1/users", "") != 200 {
		t.Fatal("inside the tenant the operator sees what an administrator sees")
	}
	if c := code(ops, "POST", e.ts.URL+"/api/v1/users", `{"name":"sneak","password":"`+pw+`","group":"user"}`); c != 403 {
		t.Fatalf("read-only means read-only: %d", c)
	}
	if c := code(ops, "GET", e.ts.URL+"/api/v1/operator/tenants", ""); c != 403 {
		t.Fatalf("the console is not available from inside a tenant: %d", c)
	}
	if c := code(ops, "PUT", e.ts.URL+"/api/v1/support-access", `{"mode":"allow"}`); c != 403 {
		t.Fatalf("an operator inside a tenant cannot widen its own access: %d", c)
	}
	if c := code(ops, "POST", e.ts.URL+"/api/v1/support-access/grant", `{"minutes":600}`); c != 403 {
		t.Fatalf("nor grant itself more time: %d", c)
	}
	// the customer can see that it happened
	_, b = do(bob, "GET", e.ts.URL+"/api/v1/audit", "")
	if !strings.Contains(string(b), `"action":"support.enter"`) || !strings.Contains(string(b), `"actor":"ops"`) || !strings.Contains(string(b), "read-only") {
		t.Fatalf("support access is never invisible: %s", b)
	}
	// leave
	if c := code(ops, "POST", e.ts.URL+"/api/v1/operator/leave", ``); c != 200 {
		t.Fatal(c)
	}
	_, b = do(ops, "GET", e.ts.URL+"/api/v1/me", "")
	if jsonOf(b)["tenant"] != "operator" || jsonOf(b)["acting"] != nil {
		t.Fatalf("back as the operator: %s", b)
	}
	// with write access every change is on record, with the operator's name
	do(ops, "POST", e.ts.URL+"/api/v1/operator/tenants/initech/enter", `{"write":true}`)
	if c := code(ops, "POST", e.ts.URL+"/api/v1/users", `{"name":"helped","password":"`+pw+`","group":"user"}`); c != 200 {
		t.Fatalf("%d", c)
	}
	code(ops, "POST", e.ts.URL+"/api/v1/operator/leave", ``)
	_, b = do(bob, "GET", e.ts.URL+"/api/v1/audit", "")
	if !strings.Contains(string(b), "support.POST") || !strings.Contains(string(b), "/api/v1/users") || !strings.Contains(string(b), `"acting":true`) {
		t.Fatalf("a change made by an operator inside the tenant is recorded: %s", b)
	}
	// the customer can switch access off, and revoke a grant
	if c := code(bob, "PUT", e.ts.URL+"/api/v1/support-access", `{"mode":"off"}`); c != 200 {
		t.Fatal(c)
	}
	if r, b := do(ops, "POST", e.ts.URL+"/api/v1/operator/tenants/initech/enter", `{}`); r.StatusCode != 400 || !strings.Contains(string(b), "switched support access off") {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	code(bob, "PUT", e.ts.URL+"/api/v1/support-access", `{"mode":"ask"}`)
	code(bob, "POST", e.ts.URL+"/api/v1/support-access/grant", `{"minutes":60}`)
	code(bob, "DELETE", e.ts.URL+"/api/v1/support-access/grant", ``)
	if c := code(ops, "POST", e.ts.URL+"/api/v1/operator/tenants/initech/enter", `{}`); c != 400 {
		t.Fatalf("a revoked grant is gone: %d", c)
	}
	if code(ops, "POST", e.ts.URL+"/api/v1/operator/tenants/operator/enter", `{}`) != 400 || code(ops, "POST", e.ts.URL+"/api/v1/operator/tenants/nobody/enter", `{}`) != 404 {
		t.Fatal("cannot enter the operator tenant or one that does not exist")
	}
	// a plain user cannot touch the setting
	if c := code(e.as(t, "acme-user"), "PUT", e.ts.URL+"/api/v1/support-access", `{"mode":"allow"}`); c != 403 {
		t.Fatal(c)
	}
	if c := code(bob, "PUT", e.ts.URL+"/api/v1/support-access", `{"mode":"sometimes"}`); c != 400 {
		t.Fatal(c)
	}
}

func TestOffboardingThroughTheConsole(t *testing.T) {
	e := tenServer(t)
	e.licence(t, license.Operator)
	ops := e.as(t, "ops")
	do(ops, "POST", e.ts.URL+"/api/v1/operator/tenants", `{"id":"gone","name":"Gone AB","admin_user":"gary","admin_password":"`+pw+`","contact":"gary@gone.example"}`)
	gary := e.as(t, "gary")
	code(gary, "POST", e.ts.URL+"/api/v1/dashboards", `{"name":"mine","description":"","panels":[]}`)
	// export: a zip without secrets
	r, b := do(ops, "GET", e.ts.URL+"/api/v1/operator/tenants/gone/export", "")
	if r.StatusCode != 200 || r.Header.Get("Content-Type") != "application/zip" || !strings.Contains(r.Header.Get("Content-Disposition"), "lumen-tenant-gone-") {
		t.Fatalf("%d %v", r.StatusCode, r.Header)
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	all, names := "", map[string]bool{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		x, _ := io.ReadAll(rc)
		rc.Close()
		all += string(x)
		names[f.Name] = true
	}
	if !names["tenant.json"] || !names["users.json"] || !names["dashboards.json"] || strings.Contains(all, `"hash"`) {
		t.Fatalf("an export has the documents and never a password hash: %v", names)
	}
	// removal must be confirmed with the id
	if c := code(ops, "POST", e.ts.URL+"/api/v1/operator/tenants/gone/offboard", `{"confirm":"wrong"}`); c != 400 {
		t.Fatalf("%d", c)
	}
	if c := code(ops, "POST", e.ts.URL+"/api/v1/operator/tenants/operator/offboard", `{"confirm":"operator"}`); c != 400 {
		t.Fatal("the operator tenant cannot be removed")
	}
	if c := code(ops, "POST", e.ts.URL+"/api/v1/operator/tenants/gone/offboard", `{"confirm":"gone"}`); c != 202 {
		t.Fatalf("%d", c)
	}
	var d map[string]any
	for i := 0; i < 50; i++ {
		_, b = do(ops, "GET", e.ts.URL+"/api/v1/operator/tenants/gone", "")
		d = jsonOf(b)
		if d["tenant"].(map[string]any)["status"] == "deleted" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	tn := d["tenant"].(map[string]any)
	if tn["status"] != "deleted" || tn["offboard"].(map[string]any)["state"] != "done" || tn["contact"] != nil {
		t.Fatalf("removed, with a tombstone and without personal details: %s", b)
	}
	if len(e.pg.purged) != 1 || e.pg.purged[0] != "gone" {
		t.Fatalf("the telemetry of this tenant only: %v", e.pg.purged)
	}
	if code(gary, "GET", e.ts.URL+"/api/v1/me", "") == 200 {
		t.Fatal("its sessions end with its users")
	}
	if r := post(client(), e.ts.URL+"/api/v1/login", `{"username":"gary","password":"`+pw+`"}`); r.StatusCode != 401 {
		t.Fatalf("its users are gone: %d", r.StatusCode)
	}
	if c := code(ops, "GET", e.ts.URL+"/api/v1/operator/tenants/gone/export", ""); c != 404 {
		t.Fatal("a removed tenant cannot be exported")
	}
	if c := code(ops, "POST", e.ts.URL+"/api/v1/operator/tenants", `{"id":"gone","name":"again","admin_user":"gary2"}`); c != 400 {
		t.Fatal("the name stays taken")
	}
	if e.ten.Tenants.Count("acme", "users") != 2 {
		t.Fatal("other tenants are untouched")
	}
	_, b = do(ops, "GET", e.ts.URL+"/api/v1/operator/audit?tenant=gone", "")
	if !strings.Contains(string(b), "tenant.offboard") || !strings.Contains(string(b), "tenant.export") {
		t.Fatalf("the audit entries of a removed tenant are kept: %s", b)
	}
}

func infoBody(host, role string) string {
	return `{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"lumen-agent"}},{"key":"host.name","value":{"stringValue":"` + host + `"}}]},"scopeMetrics":[{"metrics":[{"name":"lumen_agent_info","gauge":{"dataPoints":[{"timeUnixNano":"1700000000000000000","asDouble":1,"attributes":[{"key":"role","value":{"stringValue":"` + role + `"}}]}]}}]}]}]}`
}

func TestAnAgentThatOnlyChecksInstancesIsNotACountedHost(t *testing.T) {
	e := tenServer(t)
	e.licence(t, license.Operator)
	ops := e.as(t, "ops")
	if c := code(ops, "PUT", e.ts.URL+"/api/v1/operator/tenants/acme", `{"name":"acme","quotas":{"hosts":1,"hard":true}}`); c != 200 {
		t.Fatal(c)
	}
	if e.ingest("acme", "/v1/metrics", metricsBody("web1")).StatusCode != 200 {
		t.Fatal("the one host the plan allows")
	}
	// a checker says what it is, and then sends the instance metrics it collects: neither counts as a host
	if r := e.ingest("acme", "/v1/metrics", infoBody("nc-checker", "checker")); r.StatusCode != 200 {
		t.Fatalf("a checker is let in although the host limit is reached: %d", r.StatusCode)
	}
	if r := e.ingest("acme", "/v1/metrics", metricsBody("nc-checker")); r.StatusCode != 200 {
		t.Fatalf("also when a batch has only its instance metrics: %d", r.StatusCode)
	}
	if n := e.ten.Meter.ActiveHosts("acme"); n != 1 {
		t.Fatalf("one host is counted, not two: %d", n)
	}
	// a second real host is still refused
	if r := e.ingest("acme", "/v1/metrics", metricsBody("web2")); r.StatusCode != 403 {
		t.Fatalf("a second machine is over the limit: %d", r.StatusCode)
	}
	// one that was counted before it said what it is stops being counted
	if e.ten.Tenants.Count("acme", "users") == 0 {
		t.Fatal("setup")
	}
	e.ten.Meter.Record("acme", "metrics", 1, 1, []string{"old-checker"})
	e.ten.Meter.MarkChecker("acme", "old-checker")
	if n := e.ten.Meter.ActiveHosts("acme"); n != 1 {
		t.Fatalf("%d", n)
	}
}
