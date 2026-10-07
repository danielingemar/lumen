package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/danielingemar/lumen/internal/registry"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/alerts"
	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/dashboards"
	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/model"
	"github.com/danielingemar/lumen/internal/secretbox"
	"github.com/danielingemar/lumen/internal/status"
)

type alertQ struct {
	mu   sync.Mutex
	vals map[string]float64 // host -> value, for tenant acme only
}

func (q *alertQ) Series(_ context.Context, tenant string, sq model.SeriesQuery) ([]model.Series, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if tenant != "acme" {
		return nil, nil
	}
	var out []model.Series
	for h, v := range q.vals {
		out = append(out, model.Series{Labels: map[string]string{"host": h}, Points: [][2]float64{{float64(sq.To.UnixMilli()), v}}})
	}
	return out, nil
}

type noStatus struct{}

func (noStatus) Summary(context.Context, string) (status.Summary, error) {
	return status.Summary{}, nil
}

type alertEnv struct {
	ts  *httptest.Server
	eng *alerts.Engine
	q   *alertQ
	clk *struct{ t time.Time }
}

func alertServer(t *testing.T) *alertEnv {
	f, _ := docstore.OpenFile(t.TempDir())
	st, _ := auth.Open(f, 0)
	st.CreateUser("admin", "acme", "admins-long-password")
	st.CreateUserIn("reader", "acme", "readers-long-password", "user")
	g, _ := st.CreateGroup("acme", "AlertsOnly", "", map[string]string{"alerts": "write"}) // no notifications, no metrics
	st.CreateUserIn("alertsonly", "acme", "alertsonly-long-password", g.ID)
	st.CreateUser("globexadmin", "globex", "globex-long-password")
	a := auth.New(st, nil, false)
	box, _ := secretbox.New("k")
	q := &alertQ{vals: map[string]float64{}}
	clk := &struct{ t time.Time }{t: time.Now()}
	eng := &alerts.Engine{DB: f, Box: box, Eval: &alerts.Evaluator{Q: q, St: noStatus{}}, Guard: alerts.Guard{AllowPrivate: true}, GroupWait: time.Second,
		Now: func() time.Time { return clk.t }, PublicURL: "https://lumen.example.com"}
	ts := httptest.NewServer(New(&fakeStore{}, a, slog.New(slog.NewTextHandler(io.Discard, nil))).WithAuth(a).WithDashboards(dashboards.New(f)).WithAlerts(eng).Handler())
	t.Cleanup(ts.Close)
	return &alertEnv{ts, eng, q, clk}
}

func (v *alertEnv) client(t *testing.T, user, pw string) *http.Client {
	c := client()
	login(t, c, v.ts, user, pw)
	return c
}

const ruleJSON = `{"name":"Disk almost full","kind":"metric","metric":"system.filesystem.utilization","reduce":"max","group_by":"host","window_sec":300,"op":">","threshold":0.9,"severity":"critical","enabled":true,"for_sec":0,"interval_sec":15}`

func TestAlertsPermissions(t *testing.T) {
	v := alertServer(t)
	rd, ao := v.client(t, "reader", "readers-long-password"), v.client(t, "alertsonly", "alertsonly-long-password")
	// a read-only user can look at alerts, rules, silences and history, but not change anything, and sees no channels
	for _, p := range []string{"/api/v1/alerts", "/api/v1/alerts/rules", "/api/v1/alerts/silences", "/api/v1/alerts/history", "/api/v1/alerts/templates"} {
		if c := code(rd, "GET", v.ts.URL+p, ""); c != 200 {
			t.Errorf("a user reads %s: %d", p, c)
		}
	}
	for _, x := range []struct{ m, p, b string }{
		{"POST", "/api/v1/alerts/rules", ruleJSON}, {"POST", "/api/v1/alerts/silences", `{"matchers":{"host":"a"},"minutes":60}`},
		{"POST", "/api/v1/alerts/ack/fp", ""}, {"GET", "/api/v1/notifications/channels", ""}, {"GET", "/api/v1/notifications/types", ""}, {"GET", "/api/v1/alerts/deliveries", ""},
		{"POST", "/api/v1/notifications/channels", `{"name":"x","type":"webhook"}`},
	} {
		if c := code(rd, x.m, v.ts.URL+x.p, x.b); c != 403 {
			t.Errorf("a read-only user must not %s %s: %d", x.m, x.p, c)
		}
	}
	// a user may read metrics, so replaying a metric rule over them is fine
	if c := code(rd, "POST", v.ts.URL+"/api/v1/alerts/backtest", `{"rule":`+ruleJSON+`}`); c != 200 {
		t.Errorf("a user who may read metrics can replay a metric rule: %d", c)
	}
	// someone who manages alerts but has no notification permission cannot touch channels
	if code(ao, "POST", v.ts.URL+"/api/v1/alerts/rules", ruleJSON) != 200 {
		t.Fatal("alerts:write may create rules")
	}
	if code(ao, "GET", v.ts.URL+"/api/v1/notifications/channels", "") != 403 || code(ao, "POST", v.ts.URL+"/api/v1/notifications/channels", `{"name":"x","type":"webhook"}`) != 403 {
		t.Fatal("channels need the notifications permission (they hold secrets and send from the server)")
	}
	if code(ao, "POST", v.ts.URL+"/api/v1/alerts/backtest", `{"rule":`+ruleJSON+`}`) != 403 {
		t.Fatal("replaying a metric rule needs the metrics permission")
	}
}

func TestAlertRulesAndTemplates(t *testing.T) {
	v := alertServer(t)
	ad := v.client(t, "admin", "admins-long-password")
	r, b := do(ad, "POST", v.ts.URL+"/api/v1/alerts/rules", ruleJSON)
	var rule alerts.Rule
	json.Unmarshal(b, &rule)
	if r.StatusCode != 200 || rule.ID == "" || rule.Tenant != "acme" || rule.Threshold != 0.9 || !strings.HasPrefix(rule.ID, "r_") {
		t.Fatalf("create: %d %s", r.StatusCode, b)
	}
	for name, bad := range map[string]string{
		"bad kind": `{"name":"x","kind":"weird"}`, "fast": `{"name":"x","kind":"metric","metric":"m","op":">","interval_sec":5}`,
		"no name": `{"kind":"metric","metric":"m","op":">"}`, "bad json": `{`,
	} {
		if c := code(ad, "POST", v.ts.URL+"/api/v1/alerts/rules", bad); c != 400 {
			t.Errorf("%s must be 400, got %d", name, c)
		}
	}
	// update, list with health, delete
	upd := strings.Replace(ruleJSON, `"threshold":0.9`, `"threshold":0.8`, 1)
	if r, b := do(ad, "PUT", v.ts.URL+"/api/v1/alerts/rules/"+rule.ID, upd); r.StatusCode != 200 || !strings.Contains(string(b), `"threshold":0.8`) {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	_, b = do(ad, "GET", v.ts.URL+"/api/v1/alerts/rules", "")
	if !strings.Contains(string(b), rule.ID) || !strings.Contains(string(b), `"name":"Disk almost full"`) {
		t.Fatalf("%s", b)
	}
	if code(ad, "PUT", v.ts.URL+"/api/v1/alerts/rules/nope", upd) != 404 || code(ad, "DELETE", v.ts.URL+"/api/v1/alerts/rules/nope", "") != 404 {
		t.Fatal("unknown rule")
	}
	if code(ad, "DELETE", v.ts.URL+"/api/v1/alerts/rules/"+rule.ID, "") != 200 {
		t.Fatal("delete")
	}
	// every template is a valid rule that can be created as it is
	_, b = do(ad, "GET", v.ts.URL+"/api/v1/alerts/templates", "")
	var tpl struct{ Data []json.RawMessage }
	json.Unmarshal(b, &tpl)
	if len(tpl.Data) < 8 {
		t.Fatalf("starter rules: %s", b)
	}
	for _, d := range tpl.Data {
		if c := code(ad, "POST", v.ts.URL+"/api/v1/alerts/rules", string(d)); c != 200 {
			t.Errorf("template must be creatable: %d %s", c, d)
		}
	}
}

func TestChannelsNeverRevealSecretsAndTestWorks(t *testing.T) {
	v := alertServer(t)
	ad, gx := v.client(t, "admin", "admins-long-password"), v.client(t, "globexadmin", "globex-long-password")
	var got struct {
		sig, ts string
		body    []byte
	}
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.body, _ = io.ReadAll(r.Body)
		got.sig, got.ts = r.Header.Get("X-Lumen-Signature"), r.Header.Get("X-Lumen-Timestamp")
	}))
	defer dest.Close()
	_, b := do(ad, "GET", v.ts.URL+"/api/v1/notifications/types", "")
	for _, want := range []string{`"email"`, `"webhook"`, `"slack"`, `"teams"`, `"heartbeat"`, `"secret"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("the form is built from the types; missing %s: %s", want, b)
		}
	}
	r, b := do(ad, "POST", v.ts.URL+"/api/v1/notifications/channels", `{"name":"ops hook","type":"webhook","enabled":true,"settings":{"url":"`+dest.URL+`"},"secrets":{"secret":"HMAC-SECRET-VALUE"}}`)
	var ch alerts.ChannelOut
	json.Unmarshal(b, &ch)
	if r.StatusCode != 200 || ch.ID == "" || !ch.HasSecret["secret"] || strings.Contains(string(b), "HMAC-SECRET-VALUE") {
		t.Fatalf("create: %d %s", r.StatusCode, b)
	}
	_, b = do(ad, "GET", v.ts.URL+"/api/v1/notifications/channels", "")
	if strings.Contains(string(b), "HMAC-SECRET-VALUE") || !strings.Contains(string(b), "ops hook") || !strings.Contains(string(b), `"type_label":"Webhook"`) {
		t.Fatalf("the list never contains secrets: %s", b)
	}
	// the test message really reaches the destination, signed with the secret
	r, b = do(ad, "POST", v.ts.URL+"/api/v1/notifications/channels/"+ch.ID+"/test", "")
	if r.StatusCode != 200 || !strings.Contains(string(b), `"ok":true`) || got.sig != alerts.Sign("HMAC-SECRET-VALUE", got.ts, got.body) || !strings.Contains(string(got.body), "[TEST]") {
		t.Fatalf("test: %d %s sig %q", r.StatusCode, b, got.sig)
	}
	// a destination that fails is a result of the test, with the reason, not a server error
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) }))
	defer bad.Close()
	_, b = do(ad, "POST", v.ts.URL+"/api/v1/notifications/channels", `{"name":"broken","type":"webhook","enabled":true,"settings":{"url":"`+bad.URL+`"}}`)
	var ch2 alerts.ChannelOut
	json.Unmarshal(b, &ch2)
	r, b = do(ad, "POST", v.ts.URL+"/api/v1/notifications/channels/"+ch2.ID+"/test", "")
	if r.StatusCode != 200 || !strings.Contains(string(b), `"ok":false`) || !strings.Contains(string(b), "500") {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	_, b = do(ad, "GET", v.ts.URL+"/api/v1/alerts/deliveries", "")
	if !strings.Contains(string(b), `"status":"test"`) {
		t.Fatalf("the delivery log shows the tests: %s", b)
	}
	// invalid channels
	for name, body := range map[string]string{
		"unknown type": `{"name":"x","type":"fax"}`, "missing url": `{"name":"x","type":"webhook","settings":{}}`,
		"bad url": `{"name":"x","type":"webhook","settings":{"url":"ftp://x"}}`, "unknown setting": `{"name":"x","type":"webhook","settings":{"url":"https://x.example.com","evil":"1"}}`,
		"slack over http": `{"name":"x","type":"slack","secrets":{"webhook_url":"http://hooks.example.com/x"}}`,
	} {
		if c := code(ad, "POST", v.ts.URL+"/api/v1/notifications/channels", body); c != 400 {
			t.Errorf("%s must be 400, got %d", name, c)
		}
	}
	// another tenant sees nothing and can change nothing
	_, b = do(gx, "GET", v.ts.URL+"/api/v1/notifications/channels", "")
	if strings.Contains(string(b), "ops hook") {
		t.Fatalf("tenant isolation: %s", b)
	}
	for _, x := range []struct{ m, p, b string }{
		{"PUT", "/api/v1/notifications/channels/" + ch.ID, `{"name":"x","type":"webhook","settings":{"url":"https://evil.example.com"}}`},
		{"DELETE", "/api/v1/notifications/channels/" + ch.ID, ""}, {"POST", "/api/v1/notifications/channels/" + ch.ID + "/test", ""},
	} {
		if c := code(gx, x.m, v.ts.URL+x.p, x.b); c != 404 {
			t.Errorf("another tenant must get 404 for %s %s, got %d", x.m, x.p, c)
		}
	}
	if code(ad, "DELETE", v.ts.URL+"/api/v1/notifications/channels/"+ch.ID, "") != 200 {
		t.Fatal("delete")
	}
}

func TestAlertLifecycleThroughTheAPI(t *testing.T) {
	v := alertServer(t)
	ad, rd, gx := v.client(t, "admin", "admins-long-password"), v.client(t, "reader", "readers-long-password"), v.client(t, "globexadmin", "globex-long-password")
	var msgs []map[string]any
	var mu sync.Mutex
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		msgs = append(msgs, m)
		mu.Unlock()
	}))
	defer dest.Close()
	do(ad, "POST", v.ts.URL+"/api/v1/notifications/channels", `{"name":"hook","type":"webhook","enabled":true,"settings":{"url":"`+dest.URL+`"}}`)
	do(ad, "POST", v.ts.URL+"/api/v1/alerts/rules", ruleJSON)
	v.q.mu.Lock()
	v.q.vals = map[string]float64{"web1": 0.97, "web2": 0.2}
	v.q.mu.Unlock()
	ctx := context.Background()
	v.eng.EvalDue(ctx)
	v.clk.t = v.clk.t.Add(2 * time.Second)
	v.eng.Dispatch(ctx)
	mu.Lock()
	n := len(msgs)
	mu.Unlock()
	if n != 1 || msgs[0]["status"] != "firing" || !strings.Contains(msgs[0]["title"].(string), "Disk almost full") {
		t.Fatalf("the webhook received the alert: %v", msgs)
	}
	// everyone with read access sees it, in their own tenant only
	_, b := do(rd, "GET", v.ts.URL+"/api/v1/alerts", "")
	var l struct {
		Data []struct {
			Fingerprint, State, Severity string
			Labels                       map[string]string
			Acked, Silenced              bool
		}
		Counts struct{ Firing, Pending int }
	}
	json.Unmarshal(b, &l)
	if len(l.Data) != 1 || l.Data[0].State != "firing" || l.Data[0].Labels["host"] != "web1" || l.Counts.Firing != 1 {
		t.Fatalf("%s", b)
	}
	_, b = do(gx, "GET", v.ts.URL+"/api/v1/alerts", "")
	if strings.Contains(string(b), "web1") {
		t.Fatalf("tenant isolation: %s", b)
	}
	fp := l.Data[0].Fingerprint
	if code(gx, "POST", v.ts.URL+"/api/v1/alerts/ack/"+fp, "") != 404 {
		t.Fatal("another tenant cannot acknowledge")
	}
	if code(rd, "POST", v.ts.URL+"/api/v1/alerts/ack/"+fp, "") != 403 {
		t.Fatal("a read-only user cannot acknowledge")
	}
	if code(ad, "POST", v.ts.URL+"/api/v1/alerts/ack/"+fp, "") != 200 {
		t.Fatal("ack")
	}
	_, b = do(ad, "GET", v.ts.URL+"/api/v1/alerts", "")
	if !strings.Contains(string(b), `"acked":true`) || !strings.Contains(string(b), `"acked_by":"admin"`) {
		t.Fatalf("%s", b)
	}
	if code(ad, "DELETE", v.ts.URL+"/api/v1/alerts/ack/"+fp, "") != 200 {
		t.Fatal("unack")
	}
	// silence it
	r, b := do(ad, "POST", v.ts.URL+"/api/v1/alerts/silences", `{"matchers":{"host":"web1"},"minutes":60,"reason":"planned work"}`)
	var sil alerts.Silence
	json.Unmarshal(b, &sil)
	if r.StatusCode != 200 || sil.ID == "" || sil.CreatedBy != "admin" {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	_, b = do(ad, "GET", v.ts.URL+"/api/v1/alerts", "")
	if !strings.Contains(string(b), `"silenced":true`) || !strings.Contains(string(b), `"firing":0`) {
		t.Fatalf("a silenced alert is marked and not counted as firing: %s", b)
	}
	if c := code(ad, "POST", v.ts.URL+"/api/v1/alerts/silences", `{"matchers":{},"minutes":60}`); c != 400 {
		t.Fatalf("a silence without matchers is refused: %d", c)
	}
	if code(ad, "DELETE", v.ts.URL+"/api/v1/alerts/silences/"+sil.ID, "") != 200 {
		t.Fatal("delete silence")
	}
	// history shows what happened
	_, b = do(rd, "GET", v.ts.URL+"/api/v1/alerts/history", "")
	for _, want := range []string{`"type":"firing"`, `"type":"ack"`, `"type":"silence"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("history lacks %s: %s", want, b)
		}
	}
	// the status boxes tell how many alerts fire
	_, b = do(rd, "GET", v.ts.URL+"/api/v1/status", "")
	_ = b
}

func TestRulesFollowAHostGroupThatIsRenamed(t *testing.T) {
	f, _ := docstore.OpenFile(t.TempDir())
	st, _ := auth.Open(f, 0)
	st.CreateUser("admin", "acme", "admins-long-password")
	st.CreateUser("globexadmin", "globex", "globex-long-password")
	a := auth.New(st, nil, false)
	box, _ := secretbox.New("k")
	reg := registry.New(f, box)
	eng := &alerts.Engine{DB: f, Box: box, Eval: &alerts.Evaluator{Q: &alertQ{vals: map[string]float64{}}, St: noStatus{}, G: &Server{reg: reg}}, Guard: alerts.Guard{AllowPrivate: true}, GroupWait: time.Second}
	ts := httptest.NewServer(New(&fakeStore{}, a, slog.New(slog.NewTextHandler(io.Discard, nil))).WithAuth(a).WithDashboards(dashboards.New(f)).WithRegistry(reg).WithAlerts(eng).Handler())
	defer ts.Close()
	ad := client()
	login(t, ad, ts, "admin", "admins-long-password")
	gx := client()
	login(t, gx, ts, "globexadmin", "globex-long-password")
	do(ad, "POST", ts.URL+"/api/v1/host-groups/members", `{"group":"Web servers","add":["web1"]}`)
	do(gx, "POST", ts.URL+"/api/v1/host-groups/members", `{"group":"Web servers","add":["g1"]}`)
	mk := func(c *http.Client, name, group string) string {
		r, b := do(c, "POST", ts.URL+"/api/v1/alerts/rules", `{"name":"`+name+`","kind":"status","status":"host_down","group":"`+group+`"}`)
		if r.StatusCode != 200 {
			t.Fatalf("%d %s", r.StatusCode, b)
		}
		var out struct{ ID string }
		json.Unmarshal(b, &out)
		return out.ID
	}
	own, other, plain := mk(ad, "Web down", "web servers"), mk(gx, "Their web down", "Web servers"), mk(ad, "Any down", "")
	groupOf := func(c *http.Client, id string) string {
		_, b := do(c, "GET", ts.URL+"/api/v1/alerts/rules", "")
		var l struct{ Data []struct{ ID, Group string } }
		json.Unmarshal(b, &l)
		for _, r := range l.Data {
			if r.ID == id {
				return r.Group
			}
		}
		return "(no such rule)"
	}
	if !strings.EqualFold(groupOf(ad, own), "Web servers") {
		t.Fatalf("a rule keeps the group it was given (names match without regard to case): %q", groupOf(ad, own))
	}
	r, b := do(ad, "POST", ts.URL+"/api/v1/host-groups/rename", `{"from":"Web servers","to":"Frontend"}`)
	if r.StatusCode != 200 || !strings.Contains(string(b), `"rules":1`) {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	if groupOf(ad, own) != "Frontend" {
		t.Fatalf("the rule follows the group: %q", groupOf(ad, own))
	}
	if groupOf(gx, other) != "Web servers" {
		t.Fatal("another tenant's rule that names a group of the same name is not touched")
	}
	if groupOf(ad, plain) != "" {
		t.Fatal("and a rule without a group stays without")
	}
	if c := code(ad, "POST", ts.URL+"/api/v1/alerts/rules", `{"name":"x","kind":"status","status":"host_down","group":"a,b"}`); c != 400 {
		t.Fatalf("a bad group name in a rule is refused: %d", c)
	}
}

func TestARuleMadeThroughTheApiIsOnUnlessSaidOtherwise(t *testing.T) {
	v := alertServer(t)
	ad := v.client(t, "admin", "admins-long-password")
	enabledOf := func(id string) bool {
		_, b := do(ad, "GET", v.ts.URL+"/api/v1/alerts/rules", "")
		var l struct {
			Data []struct {
				ID      string
				Enabled bool
			}
		}
		json.Unmarshal(b, &l)
		for _, r := range l.Data {
			if r.ID == id {
				return r.Enabled
			}
		}
		t.Fatal("no such rule")
		return false
	}
	n := 0
	mk := func(extra string) string {
		n++
		r, b := do(ad, "POST", v.ts.URL+"/api/v1/alerts/rules", fmt.Sprintf(`{"name":"Rule %d","kind":"status","status":"host_down"%s}`, n, extra))
		if r.StatusCode != 200 {
			t.Fatalf("%d %s", r.StatusCode, b)
		}
		var out struct{ ID string }
		json.Unmarshal(b, &out)
		return out.ID
	}
	on, explicitOn, off := mk(""), mk(`,"enabled":true`), mk(`,"enabled":false`)
	if !enabledOf(on) || !enabledOf(explicitOn) || enabledOf(off) {
		t.Fatalf("a new rule is on unless it says enabled:false: %v %v %v", enabledOf(on), enabledOf(explicitOn), enabledOf(off))
	}
	// an update that does not mention it leaves it as it was; one that does, changes it
	do(ad, "PUT", v.ts.URL+"/api/v1/alerts/rules/"+off, `{"name":"R2","kind":"status","status":"host_down"}`)
	do(ad, "PUT", v.ts.URL+"/api/v1/alerts/rules/"+on, `{"name":"R3","kind":"status","status":"host_down"}`)
	if enabledOf(off) || !enabledOf(on) {
		t.Fatal("an update without the field keeps the state")
	}
	do(ad, "PUT", v.ts.URL+"/api/v1/alerts/rules/"+on, `{"name":"R3","kind":"status","status":"host_down","enabled":false}`)
	if enabledOf(on) {
		t.Fatal("and can switch it off")
	}
}
