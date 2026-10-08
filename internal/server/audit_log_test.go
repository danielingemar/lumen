package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/alerts"
	"github.com/danielingemar/lumen/internal/audit"
	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/dashboards"
	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/registry"
	"github.com/danielingemar/lumen/internal/secretbox"
)

type auditEnv struct {
	ts  *httptest.Server
	log *audit.Log
	ad  *http.Client
	rd  *http.Client
	gx  *http.Client
}

func newAuditEnv(t *testing.T) *auditEnv {
	f, _ := docstore.OpenFile(t.TempDir())
	st, _ := auth.Open(f, 0)
	st.CreateUser("admin", "acme", "admins-long-password")
	st.CreateUserIn("reader", "acme", "readers-long-password", "user")
	st.CreateUser("globexadmin", "globex", "globex-long-password")
	a := auth.New(st, nil, false)
	box, _ := secretbox.New("k")
	reg := registry.New(f, box)
	eng := &alerts.Engine{DB: f, Box: box, Eval: &alerts.Evaluator{Q: &alertQ{vals: map[string]float64{}}, St: noStatus{}, G: &Server{reg: reg}}, Guard: alerts.Guard{AllowPrivate: true}, GroupWait: time.Second}
	al := audit.New(f)
	srv := New(&fakeStore{}, a, slog.New(slog.NewTextHandler(io.Discard, nil))).WithAuth(a).WithDashboards(dashboards.New(f)).WithRegistry(reg).WithAlerts(eng).WithAudit(al)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	e := &auditEnv{ts: ts, log: al, ad: client(), rd: client(), gx: client()}
	login(t, e.ad, ts, "admin", "admins-long-password")
	login(t, e.rd, ts, "reader", "readers-long-password")
	login(t, e.gx, ts, "globexadmin", "globex-long-password")
	failedLogins.Lock()
	failedLogins.at = map[string]time.Time{}
	failedLogins.Unlock()
	return e
}

func (e *auditEnv) entries(t *testing.T, c *http.Client, query string) []audit.Entry {
	_, b := do(c, "GET", e.ts.URL+"/api/v1/audit-log"+query, "")
	var out struct{ Data []audit.Entry }
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s", b)
	}
	return out.Data
}

func has(es []audit.Entry, sub string) bool {
	for _, e := range es {
		if strings.Contains(e.Summary, sub) {
			return true
		}
	}
	return false
}

func TestEveryChangeIsOnRecord(t *testing.T) {
	e := newAuditEnv(t)
	// the sign-ins of the setup are there already
	if es := e.entries(t, e.ad, ""); !has(es, "signed in") {
		t.Fatalf("a sign-in is recorded: %+v", es)
	}
	// changes, by what they did
	do(e.ad, "POST", e.ts.URL+"/api/v1/users", `{"name":"alice","password":"alice-very-secret-pw-1","group":"user"}`)
	do(e.ad, "POST", e.ts.URL+"/api/v1/host-groups/members", `{"group":"Web","add":["web1","web2"],"remove":["web9"]}`)
	do(e.ad, "PUT", e.ts.URL+"/api/v1/hosts/web1", `{"display_name":"Primary web","systemd":true,"containers":true}`)
	do(e.ad, "POST", e.ts.URL+"/api/v1/dashboards", `{"name":"=HYPERLINK(\"http://evil\")","panels":[]}`)
	do(e.ad, "POST", e.ts.URL+"/api/v1/alerts/rules", `{"name":"Disk","kind":"status","status":"host_down"}`)
	do(e.ad, "POST", e.ts.URL+"/api/v1/host-groups/rename", `{"from":"Web","to":"Frontend"}`)
	es := e.entries(t, e.ad, "")
	for _, want := range []string{"created the user alice", "changed the host group Web (2 added, 1 removed)", "changed the settings of the host web1", "created the dashboard", "created the alert rule Disk", "renamed the host group Web to Frontend"} {
		if !has(es, want) {
			t.Errorf("%q is missing: %+v", want, summaries(es))
		}
	}
	for _, x := range es {
		if x.Action == "user.create" && (x.Actor != "admin" || x.Via != "password" || x.IP != "127.0.0.1" || x.Tenant != "acme") {
			t.Errorf("who, how and from where: %+v", x)
		}
	}
	// a password is never in the log, whatever way it is looked at
	raw, _ := json.Marshal(es)
	for _, secret := range []string{"alice-very-secret-pw-1", "admins-long-password", "readers-long-password"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%q is in the audit log", secret)
		}
	}
	// a password reset says that it was reset and not to what
	do(e.ad, "PUT", e.ts.URL+"/api/v1/users/alice", `{"password":"another-secret-pw-22"}`)
	es = e.entries(t, e.ad, "")
	raw, _ = json.Marshal(es)
	if !has(es, "reset the password of alice") || strings.Contains(string(raw), "another-secret-pw-22") {
		t.Fatalf("%v", summaries(es))
	}
	// what failed, what is only read and what was refused is not a change
	before := len(e.entries(t, e.ad, ""))
	do(e.ad, "POST", e.ts.URL+"/api/v1/users", `{"name":"bad name!","password":"x"}`) // refused: 400
	do(e.ad, "GET", e.ts.URL+"/api/v1/hosts", "")
	do(e.rd, "POST", e.ts.URL+"/api/v1/host-groups/members", `{"group":"x","add":["web1"]}`) // not allowed: 403
	if after := len(e.entries(t, e.ad, "")); after != before {
		t.Fatalf("only changes that went through are recorded: %d -> %d: %v", before, after, summaries(e.entries(t, e.ad, "")))
	}
	// another tenant sees only its own
	do(e.gx, "POST", e.ts.URL+"/api/v1/host-groups/members", `{"group":"Theirs","add":["g1"]}`)
	if es = e.entries(t, e.gx, ""); has(es, "alice") || !has(es, "Theirs") {
		t.Fatalf("%v", summaries(es))
	}
	if has(e.entries(t, e.ad, ""), "Theirs") {
		t.Fatal("and the other way round")
	}
	// who may read it
	if c := code(e.rd, "GET", e.ts.URL+"/api/v1/audit-log", ""); c != 403 {
		t.Fatalf("a user without the settings permission: %d", c)
	}
}

func summaries(es []audit.Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Actor+": "+e.Summary)
	}
	return out
}

func TestSignInsAreOnRecord(t *testing.T) {
	e := newAuditEnv(t)
	bad := func(user string) {
		c := client()
		do(c, "POST", e.ts.URL+"/api/v1/login", `{"username":"`+user+`","password":"wrong-password-here"}`)
	}
	bad("admin")
	bad("admin") // the same person from the same place within a minute: once
	bad("nosuchperson")
	es := e.entries(t, e.ad, "")
	n := 0
	for _, x := range es {
		if x.Action == "login.failed" && x.Actor == "admin" {
			n++
		}
		if x.Actor == "nosuchperson" {
			t.Fatal("a made-up name belongs to no tenant: it is not in anybody's log")
		}
	}
	if n != 1 {
		t.Fatalf("a failed sign-in is recorded once a minute for each person and place, so that guessing does not fill the log: %d", n)
	}
	if !has(es, "signed in") {
		t.Fatal("and a successful one")
	}
}

func TestTheLogCanBeSearchedAndExported(t *testing.T) {
	e := newAuditEnv(t)
	do(e.ad, "POST", e.ts.URL+"/api/v1/host-groups/members", `{"group":"Alpha","add":["web1"]}`)
	do(e.ad, "POST", e.ts.URL+"/api/v1/host-groups/members", `{"group":"Beta","add":["web2"]}`)
	do(e.ad, "POST", e.ts.URL+"/api/v1/dashboards", `{"name":"=HYPERLINK(\"http://evil\")","panels":[]}`)
	if es := e.entries(t, e.ad, "?q=alpha"); len(es) != 1 || !strings.Contains(es[0].Summary, "Alpha") {
		t.Fatalf("a text search without regard to case: %v", summaries(es))
	}
	if es := e.entries(t, e.ad, "?actor=ADMIN&q=beta"); len(es) != 1 {
		t.Fatalf("%v", summaries(e.entries(t, e.ad, "?actor=admin&q=beta")))
	}
	if es := e.entries(t, e.ad, "?actor=nobody"); len(es) != 0 {
		t.Fatal("by who")
	}
	if es := e.entries(t, e.ad, "?limit=2"); len(es) != 2 {
		t.Fatalf("limit: %d", len(es))
	}
	day := time.Now().UTC().Format("2006-01-02")
	if es := e.entries(t, e.ad, "?from="+day+"&to="+day); len(es) < 3 {
		t.Fatalf("a day is the whole day: %d", len(es))
	}
	if es := e.entries(t, e.ad, "?to=2020-01-01"); len(es) != 0 {
		t.Fatal("to")
	}
	if es := e.entries(t, e.ad, ""); es[0].Time.Before(es[len(es)-1].Time) {
		t.Fatal("newest first")
	}
	r, b := do(e.ad, "GET", e.ts.URL+"/api/v1/audit-log?format=csv", "")
	body := string(b)
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/csv") || !strings.Contains(r.Header.Get("Content-Disposition"), "lumen-audit-log.csv") || !strings.HasPrefix(body, "time_utc,who,via,ip,what,action,target,detail") {
		t.Fatalf("%d %q", r.StatusCode, body[:80])
	}
	if strings.Contains(body, ",=HYPERLINK") || !strings.Contains(body, "'=HYPERLINK") {
		t.Fatalf("a spreadsheet must not run a name that starts like a formula:\n%s", body)
	}
}

func TestOldEntriesAreForgotten(t *testing.T) {
	f, _ := docstore.OpenFile(t.TempDir())
	l := audit.New(f)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { return now.AddDate(0, 0, -400) }
	l.Add(audit.Entry{Tenant: "acme", Actor: "a", Summary: "old"})
	l.Now = func() time.Time { return now.AddDate(0, 0, -10) }
	l.Add(audit.Entry{Tenant: "acme", Actor: "a", Summary: "recent"})
	l.Now = func() time.Time { return now }
	l.Retain(10000, 365*24*time.Hour)
	if es := l.List("acme", 0); len(es) != 1 || es[0].Summary != "recent" {
		t.Fatalf("%+v", es)
	}
	for i := 0; i < 5; i++ {
		l.Add(audit.Entry{Tenant: "acme", Actor: "a", Summary: "more"})
	}
	l.Retain(3, 0)
	if es := l.List("acme", 0); len(es) != 3 {
		t.Fatalf("at most 3 per tenant: %d", len(es))
	}
}

// Every route that changes something must go through the guard that records it. A route added later that does not is found here.
func TestNoChangingRouteSlipsPastTheRecord(t *testing.T) {
	re := regexp.MustCompile(`mux\.Handle(?:Func)?\("(POST|PUT|DELETE|PATCH) ([^"]+)",\s*([^\n]*)`)
	guarded := []string{"s.need(", "s.needAny(", "s.sessionOnly(", "s.op(", "s.gate(", "s.authed("}
	allowed := map[string]bool{"/api/v1/login": true, "/api/v1/logout": true} // recorded by their own code
	files, _ := filepath.Glob("*.go")
	more, _ := filepath.Glob("../../ee/*.go")
	n := 0
	for _, f := range append(files, more...) {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			n++
			ok := allowed[m[2]]
			for _, g := range guarded {
				if strings.HasPrefix(strings.TrimSpace(m[3]), g) {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s %s (in %s) does not go through a guard that records the change", m[1], m[2], f)
			}
		}
	}
	if n < 40 {
		t.Fatalf("only %d changing routes were found: the pattern no longer matches how they are registered", n)
	}
}
