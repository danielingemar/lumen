//go:build enterprise

package ee

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/alerts"
	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/model"
	"github.com/danielingemar/lumen/internal/secretbox"
	"github.com/danielingemar/lumen/internal/status"
)

var t0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func deps() alerts.Deps {
	g := alerts.Guard{AllowPrivate: true}
	return alerts.Deps{HTTP: g.Client(5 * time.Second), Dial: g.Dial, Now: func() time.Time { return t0 }}
}

func view(host, ref string) alerts.AlertView {
	return alerts.AlertView{Fingerprint: "fp-" + host, RuleName: "Disk almost full", Severity: "critical", State: "firing", Labels: map[string]string{"host": host, "mountpoint": "/data"}, Value: 95.2, Since: t0, Annotation: "Free some space", Ref: ref}
}

func message(status string, vs ...alerts.AlertView) alerts.Message {
	title, body := alerts.Compose(status, vs, "https://lumen.example.com/#/alerts")
	return alerts.Message{Tenant: "acme", Channel: "x", Status: status, Title: title, Body: body, Link: "https://lumen.example.com/#/alerts", Alerts: vs}
}

type call struct {
	Method, Path, Query, Auth string
	Body                      map[string]any
}

type fake struct {
	mu    sync.Mutex
	calls []call
	reply func(c call) (int, string)
}

func (f *fake) server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c := call{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization")}
		json.Unmarshal(b, &c.Body)
		f.mu.Lock()
		f.calls = append(f.calls, c)
		reply := f.reply
		f.mu.Unlock()
		code, body := 200, "{}"
		if reply != nil {
			code, body = reply(c)
		}
		w.WriteHeader(code)
		io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (f *fake) last() call { f.mu.Lock(); defer f.mu.Unlock(); return f.calls[len(f.calls)-1] }
func (f *fake) n() int     { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

func TestRegisteredOnlyInTheEnterpriseBuild(t *testing.T) {
	var got []string
	for _, n := range alerts.Default.Types() {
		got = append(got, n.Type())
	}
	have := strings.Join(got, ",")
	for _, want := range []string{"email", "webhook", "slack", "teams", "heartbeat", "pagerduty", "opsgenie", "jira", "servicenow"} {
		if !strings.Contains(have, want) {
			t.Errorf("the enterprise build offers %s: %s", want, have)
		}
	}
	core := alerts.NewRegistry()
	alerts.RegisterCore(core)
	for _, n := range core.Types() {
		if strings.Contains("pagerduty opsgenie jira servicenow", n.Type()) {
			t.Fatalf("the core must not contain %s", n.Type())
		}
	}
}

func TestPagerDuty(t *testing.T) {
	f := &fake{}
	srv := f.server(t)
	cfg := alerts.Config{Secrets: map[string]string{"routing_key": "RK-123"}, Settings: map[string]string{"api_url": srv.URL + "/v2/enqueue"}}
	if _, err := (pagerDuty{}).Send(context.Background(), deps(), cfg, message("firing", view("web1", ""), view("web2", ""))); err != nil {
		t.Fatal(err)
	}
	if f.n() != 2 {
		t.Fatalf("one event per alert, so each can be resolved on its own: %d", f.n())
	}
	c := f.calls[0]
	pl, _ := c.Body["payload"].(map[string]any)
	if c.Path != "/v2/enqueue" || c.Body["routing_key"] != "RK-123" || c.Body["event_action"] != "trigger" || c.Body["dedup_key"] != "fp-web1" || pl["severity"] != "critical" || !strings.Contains(pl["summary"].(string), "host=web1") || pl["source"] != "Lumen" {
		t.Fatalf("trigger: %+v", c)
	}
	if links, _ := c.Body["links"].([]any); len(links) != 1 {
		t.Fatalf("a link back to Lumen: %+v", c.Body)
	}
	if _, err := (pagerDuty{}).Send(context.Background(), deps(), cfg, message("resolved", view("web1", ""))); err != nil {
		t.Fatal(err)
	}
	r := f.last()
	if r.Body["event_action"] != "resolve" || r.Body["dedup_key"] != "fp-web1" || r.Body["payload"] != nil {
		t.Fatalf("resolve uses the same dedup key: %+v", r.Body)
	}
	f.reply = func(call) (int, string) { return 400, "bad routing key" }
	if _, err := (pagerDuty{}).Send(context.Background(), deps(), cfg, message("firing", view("web1", ""))); err == nil || !strings.Contains(err.Error(), "400") || strings.Contains(err.Error(), "RK-123") {
		t.Fatalf("%v", err)
	}
	if err := (pagerDuty{}).Validate(alerts.Config{}); err == nil {
		t.Fatal("the key is required")
	}
}

func TestOpsgenie(t *testing.T) {
	f := &fake{}
	srv := f.server(t)
	cfg := alerts.Config{Secrets: map[string]string{"api_key": "GK-1"}, Settings: map[string]string{"api_url": srv.URL}}
	if _, err := (opsgenie{}).Send(context.Background(), deps(), cfg, message("firing", view("web1", ""))); err != nil {
		t.Fatal(err)
	}
	c := f.last()
	if c.Path != "/v2/alerts" || c.Auth != "GenieKey GK-1" || c.Body["alias"] != "fp-web1" || c.Body["priority"] != "P1" || c.Body["source"] != "Lumen" || !strings.Contains(c.Body["description"].(string), "Free some space") {
		t.Fatalf("create: %+v", c)
	}
	if _, err := (opsgenie{}).Send(context.Background(), deps(), cfg, message("resolved", view("web1", ""))); err != nil {
		t.Fatal(err)
	}
	c = f.last()
	if c.Path != "/v2/alerts/fp-web1/close" || c.Query != "identifierType=alias" {
		t.Fatalf("close by alias: %+v", c)
	}
	if err := (opsgenie{}).Validate(alerts.Config{Secrets: map[string]string{"api_key": "k"}, Settings: map[string]string{"region": "asia"}}); err == nil {
		t.Fatal("region")
	}
}

func jiraFake(t *testing.T, transitions string) (*fake, alerts.Config) {
	f := &fake{}
	f.reply = func(c call) (int, string) {
		switch {
		case c.Method == "POST" && c.Path == "/rest/api/2/issue":
			return 201, `{"id":"10001","key":"OPS-` + c.Body["fields"].(map[string]any)["summary"].(string)[len("Disk almost full: host="):len("Disk almost full: host=")+4] + `"}`
		case c.Method == "GET" && strings.HasSuffix(c.Path, "/transitions"):
			return 200, transitions
		}
		return 204, ""
	}
	srv := f.server(t)
	return f, alerts.Config{Settings: map[string]string{"base_url": srv.URL, "email": "bot@example.com", "project_key": "OPS"}, Secrets: map[string]string{"api_token": "TOKEN"}}
}

func TestJiraCreatesCommentsAndCloses(t *testing.T) {
	f, cfg := jiraFake(t, `{"transitions":[{"id":"11","name":"In Progress"},{"id":"31","name":"done"}]}`)
	res, err := (jira{}).Send(context.Background(), deps(), cfg, message("firing", view("web1", "")))
	if err != nil {
		t.Fatal(err)
	}
	c := f.calls[0]
	fields := c.Body["fields"].(map[string]any)
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("bot@example.com:TOKEN"))
	if c.Auth != wantAuth || fields["project"].(map[string]any)["key"] != "OPS" || fields["issuetype"].(map[string]any)["name"] != "Task" || !strings.Contains(fields["description"].(string), "Free some space") {
		t.Fatalf("create: %+v", c)
	}
	if res.Refs["fp-web1"] != "OPS-web1" {
		t.Fatalf("the ticket key comes back so it can be updated later: %+v", res.Refs)
	}
	// still firing and the alert already has a ticket: a comment, not a new ticket
	n := f.n()
	if _, err := (jira{}).Send(context.Background(), deps(), cfg, message("firing", view("web1", "OPS-web1"))); err != nil {
		t.Fatal(err)
	}
	if f.n() != n+1 || f.last().Path != "/rest/api/2/issue/OPS-web1/comment" || !strings.Contains(f.last().Body["body"].(string), "Still firing") {
		t.Fatalf("repeat: %+v", f.last())
	}
	// resolved: a comment and the Done transition (name matched without regard to case)
	n = f.n()
	if _, err := (jira{}).Send(context.Background(), deps(), cfg, message("resolved", view("web1", "OPS-web1"))); err != nil {
		t.Fatal(err)
	}
	got := f.calls[n:]
	if len(got) != 3 || !strings.HasSuffix(got[0].Path, "/comment") || got[1].Method != "GET" || got[2].Method != "POST" || !strings.HasSuffix(got[2].Path, "/transitions") || got[2].Body["transition"].(map[string]any)["id"] != "31" {
		t.Fatalf("resolve: %+v", got)
	}
	// no such transition in the workflow: still a comment, and no error
	f2, cfg2 := jiraFake(t, `{"transitions":[{"id":"11","name":"In Progress"}]}`)
	if _, err := (jira{}).Send(context.Background(), deps(), cfg2, message("resolved", view("web1", "OPS-web1"))); err != nil {
		t.Fatalf("a workflow without Done must not make delivery fail: %v", err)
	}
	for _, c := range f2.calls {
		if c.Method == "POST" && strings.HasSuffix(c.Path, "/transitions") {
			t.Fatal("no transition to make")
		}
	}
	// a resolved alert that never got a ticket does nothing
	f3, cfg3 := jiraFake(t, "{}")
	if _, err := (jira{}).Send(context.Background(), deps(), cfg3, message("resolved", view("web1", ""))); err != nil || f3.n() != 0 {
		t.Fatal("nothing to close")
	}
	// errors tell what to check, and never contain the token
	f4 := &fake{reply: func(call) (int, string) { return 400, `{"errors":{"project":"no such project"}}` }}
	srv4 := f4.server(t)
	cfg4 := alerts.Config{Settings: map[string]string{"base_url": srv4.URL, "email": "bot@example.com", "project_key": "NOPE"}, Secrets: map[string]string{"api_token": "TOKEN"}}
	if _, err := (jira{}).Send(context.Background(), deps(), cfg4, message("firing", view("web1", ""))); err == nil || !strings.Contains(err.Error(), "project key") || strings.Contains(err.Error(), "TOKEN") {
		t.Fatalf("%v", err)
	}
	for name, c := range map[string]alerts.Config{
		"no url":      {Settings: map[string]string{"email": "a", "project_key": "OPS"}, Secrets: map[string]string{"api_token": "t"}},
		"bad project": {Settings: map[string]string{"base_url": "https://x.atlassian.net", "email": "a", "project_key": "O P S"}, Secrets: map[string]string{"api_token": "t"}},
		"no token":    {Settings: map[string]string{"base_url": "https://x.atlassian.net", "email": "a", "project_key": "OPS"}},
	} {
		if (jira{}).Validate(c) == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
}

func TestServiceNow(t *testing.T) {
	f := &fake{reply: func(c call) (int, string) {
		if c.Method == "POST" {
			return 201, `{"result":{"sys_id":"abc123","number":"INC0010001"}}`
		}
		return 200, "{}"
	}}
	srv := f.server(t)
	cfg := alerts.Config{Settings: map[string]string{"instance_url": srv.URL, "username": "svc"}, Secrets: map[string]string{"password": "pw"}}
	res, err := (serviceNow{}).Send(context.Background(), deps(), cfg, message("firing", view("web1", "")))
	if err != nil || res.Refs["fp-web1"] != "abc123" {
		t.Fatalf("%v %+v", err, res)
	}
	c := f.calls[0]
	if c.Path != "/api/now/table/incident" || c.Auth != "Basic "+base64.StdEncoding.EncodeToString([]byte("svc:pw")) || c.Body["urgency"] != "1" || c.Body["correlation_id"] != "fp-web1" {
		t.Fatalf("create: %+v", c)
	}
	if _, err := (serviceNow{}).Send(context.Background(), deps(), cfg, message("firing", view("web1", "abc123"))); err != nil {
		t.Fatal(err)
	}
	if c := f.last(); c.Method != "PATCH" || c.Path != "/api/now/table/incident/abc123" || !strings.Contains(c.Body["work_notes"].(string), "Still firing") {
		t.Fatalf("repeat: %+v", c)
	}
	if _, err := (serviceNow{}).Send(context.Background(), deps(), cfg, message("resolved", view("web1", "abc123"))); err != nil {
		t.Fatal(err)
	}
	if c := f.last(); c.Method != "PATCH" || c.Body["state"] != "6" || c.Body["close_notes"] != "Resolved in Lumen" {
		t.Fatalf("resolve: %+v", c)
	}
}

func TestInternalAddressesAreRefusedByDefault(t *testing.T) {
	f := &fake{}
	srv := f.server(t) // on 127.0.0.1
	g := alerts.Guard{}
	d := alerts.Deps{HTTP: g.Client(3 * time.Second), Dial: g.Dial, Now: func() time.Time { return t0 }}
	cfg := alerts.Config{Secrets: map[string]string{"routing_key": "k"}, Settings: map[string]string{"api_url": srv.URL}}
	if _, err := (pagerDuty{}).Send(context.Background(), d, cfg, message("firing", view("web1", ""))); err == nil || f.n() != 0 {
		t.Fatalf("an internal destination is refused and never contacted: %v", err)
	}
}

// ---- the whole path through the engine: a Jira ticket is created, kept up to date and closed ----

type onceSeries struct {
	mu  sync.Mutex
	val float64
}

func (o *onceSeries) Series(_ context.Context, _ string, q model.SeriesQuery) ([]model.Series, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return []model.Series{{Labels: map[string]string{"host": "web1"}, Points: [][2]float64{{float64(q.To.UnixMilli()), o.val}}}}, nil
}

type noStatus struct{}

func (noStatus) Summary(context.Context, string) (status.Summary, error) {
	return status.Summary{}, nil
}

func TestEngineKeepsTheJiraTicketForTheAlert(t *testing.T) {
	f, cfg := jiraFake(t, `{"transitions":[{"id":"31","name":"Done"}]}`)
	db, _ := docstore.OpenFile(t.TempDir())
	box, _ := secretbox.New("k")
	q := &onceSeries{val: 95}
	now := t0
	reg := alerts.NewRegistry()
	RegisterEnterprise(reg)
	e := &alerts.Engine{License: allowAll{}, DB: db, Box: box, Eval: &alerts.Evaluator{Q: q, St: noStatus{}}, Reg: reg, Guard: alerts.Guard{AllowPrivate: true}, Now: func() time.Time { return now }, GroupWait: time.Second, RepeatEvery: time.Hour, PublicURL: "https://lumen.example.com"}
	if _, err := e.PutChannel("acme", "", alerts.ChannelIn{Name: "tickets", Type: "jira", Enabled: true, Settings: cfg.Settings, Secrets: map[string]string{"api_token": "TOKEN"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PutRule("acme", "", alerts.Rule{Name: "Disk almost full", Kind: "metric", Metric: "m", Reduce: "last", GroupBy: "host", WindowSec: 60, Op: ">", Threshold: 90, IntervalSec: 15, Severity: "critical", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	step := func(d time.Duration) {
		now = now.Add(d)
		e.EvalDue(context.Background())
		e.Dispatch(context.Background())
	}
	step(0)
	step(2 * time.Second)
	if f.n() != 1 || f.calls[0].Path != "/rest/api/2/issue" {
		t.Fatalf("a ticket is created when the alert fires: %+v", f.calls)
	}
	// an hour later it still fires: a comment on the same ticket, not a second ticket
	step(61 * time.Minute)
	step(2 * time.Second)
	creates := 0
	for _, c := range f.calls {
		if c.Method == "POST" && c.Path == "/rest/api/2/issue" {
			creates++
		}
	}
	if creates != 1 || !strings.HasSuffix(f.last().Path, "/comment") {
		t.Fatalf("one ticket, then comments: %+v", f.calls)
	}
	// it recovers: the ticket is commented and closed
	q.mu.Lock()
	q.val = 10
	q.mu.Unlock()
	step(16 * time.Second)
	step(16 * time.Second)
	step(2 * time.Second)
	last := f.calls[len(f.calls)-1]
	if last.Method != "POST" || !strings.HasSuffix(last.Path, "/transitions") || !strings.Contains(last.Path, "OPS-web1") {
		t.Fatalf("the ticket the engine remembered is closed: %+v", f.calls)
	}
}

type allowAll struct{}

func (allowAll) Allows(string) bool { return true }

type denyAll struct{}

func (denyAll) Allows(ed string) bool { return ed == "community" }

func TestEveryEnterpriseNotifierNeedsALicence(t *testing.T) {
	db, _ := docstore.OpenFile(t.TempDir())
	box, _ := secretbox.New("k")
	reg := alerts.NewRegistry()
	alerts.RegisterCore(reg)
	RegisterEnterprise(reg)
	e := &alerts.Engine{License: denyAll{}, DB: db, Box: box, Eval: &alerts.Evaluator{}, Reg: reg}
	for _, n := range reg.Types() {
		want := "community"
		if strings.Contains("pagerduty opsgenie jira servicenow", n.Type()) {
			want = "enterprise"
		}
		if alerts.EditionOf(n) != want || e.Allowed(n) != (want == "community") {
			t.Errorf("%s: edition %s allowed %v", n.Type(), alerts.EditionOf(n), e.Allowed(n))
		}
	}
	_, err := e.PutChannel("acme", "", alerts.ChannelIn{Name: "p", Type: "pagerduty", Enabled: true, Secrets: map[string]string{"routing_key": "k"}})
	if err == nil || !strings.Contains(err.Error(), "licence") {
		t.Fatalf("PagerDuty cannot be set up without a licence: %v", err)
	}
}
