package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/model"
	"github.com/danielingemar/lumen/internal/secretbox"
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

// scriptQ answers series queries from a table: tenant -> host -> value.
type scriptQ struct {
	mu      sync.Mutex
	vals    map[string]map[string]float64
	err     error
	tenants []string
}

func (s *scriptQ) set(tenant string, vals map[string]float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vals == nil {
		s.vals = map[string]map[string]float64{}
	}
	s.vals[tenant] = vals
}

func (s *scriptQ) Series(_ context.Context, tenant string, q model.SeriesQuery) ([]model.Series, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenants = append(s.tenants, tenant)
	if s.err != nil {
		return nil, s.err
	}
	var out []model.Series
	for _, h := range []string{"db1", "web1", "web2"} {
		if v, ok := s.vals[tenant][h]; ok {
			out = append(out, model.Series{Labels: map[string]string{"host": h}, Points: [][2]float64{{float64(q.To.UnixMilli()), v}}})
		}
	}
	return out, nil
}

type rec struct {
	mu   sync.Mutex
	msgs []Message
	fail int
	refs bool
	seen []Config
}

type recNotifier struct{ r *rec }

func (recNotifier) Type() string  { return "rec" }
func (recNotifier) Label() string { return "Recorder" }
func (recNotifier) Fields() []Field {
	return []Field{{Key: "target", Label: "Target", Kind: "text", Required: true}, {Key: "token", Label: "Token", Kind: "secret"}}
}
func (recNotifier) Validate(c Config) error {
	if strings.Contains(c.Get("target"), "bad") {
		return fmt.Errorf("the target is bad")
	}
	return nil
}
func (n recNotifier) Send(_ context.Context, _ Deps, c Config, m Message) (Result, error) {
	n.r.mu.Lock()
	defer n.r.mu.Unlock()
	if n.r.fail > 0 {
		n.r.fail--
		return Result{}, errors.New("the destination is down")
	}
	n.r.msgs = append(n.r.msgs, m)
	n.r.seen = append(n.r.seen, c)
	res := Result{}
	if n.r.refs && m.Status == "firing" {
		res.Refs = map[string]string{}
		for _, a := range m.Alerts {
			res.Refs[a.Fingerprint] = "TICKET-" + a.Labels["host"]
		}
	}
	return res, nil
}

func (r *rec) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.msgs) }
func (r *rec) last() Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.msgs[len(r.msgs)-1]
}

type env struct {
	e   *Engine
	q   *scriptQ
	r   *rec
	clk *clock
	db  docstore.Backend
}

func newEnv(t *testing.T) *env {
	db, err := docstore.OpenFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return newEnvOn(t, db, &clock{t: t0})
}

func newEnvOn(t *testing.T, db docstore.Backend, clk *clock) *env {
	box, _ := secretbox.New("test-key")
	q, r := &scriptQ{}, &rec{}
	reg := NewRegistry()
	reg.Register(recNotifier{r})
	e := &Engine{DB: db, Box: box, Eval: &Evaluator{Q: q, St: fakeSt{}}, Reg: reg, Now: clk.Now, GroupWait: 30 * time.Second, RepeatEvery: 4 * time.Hour, Backoff: []time.Duration{10 * time.Second, 30 * time.Second}, PublicURL: "https://lumen.example.com"}
	return &env{e, q, r, clk, db}
}

// step moves the clock and runs one round of evaluation and delivery.
func (v *env) step(d time.Duration) {
	v.clk.add(d)
	ctx := context.Background()
	v.e.EvalDue(ctx)
	v.e.Dispatch(ctx)
}

func (v *env) channel(t *testing.T, tenant, name string, in ChannelIn) ChannelOut {
	if in.Type == "" {
		in.Type = "rec"
	}
	in.Name, in.Enabled = name, true
	if in.Settings == nil {
		in.Settings = map[string]string{"target": "x"}
	}
	c, err := v.e.PutChannel(tenant, "", in)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func diskRule() Rule {
	return Rule{Name: "Disk almost full", Kind: KindMetric, Metric: "m", Reduce: "last", GroupBy: "host", WindowSec: 60, Op: ">", Threshold: 90, IntervalSec: 15, Severity: "critical", Enabled: true}
}

func (v *env) rule(t *testing.T, tenant string, r Rule) Rule {
	out, err := v.e.PutRule(tenant, "", r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFiresGroupsOneMessageAndResolves(t *testing.T) {
	v := newEnv(t)
	v.channel(t, "acme", "ops", ChannelIn{})
	v.rule(t, "acme", diskRule())
	v.q.set("acme", map[string]float64{"web1": 95, "web2": 96, "db1": 10})
	v.step(0)
	if len(v.e.ListAlerts("acme")) != 2 {
		t.Fatalf("two alerts fire: %+v", v.e.ListAlerts("acme"))
	}
	if v.r.count() != 0 {
		t.Fatal("nothing is sent before the group wait (related alerts go in one message)")
	}
	v.step(31 * time.Second)
	if v.r.count() != 1 {
		t.Fatalf("one message for both alerts, got %d", v.r.count())
	}
	m := v.r.last()
	if m.Status != "firing" || len(m.Alerts) != 2 || m.Title != "[FIRING] Disk almost full (2 alerts)" || m.Tenant != "acme" || m.Link != "https://lumen.example.com/#/alerts" || !strings.Contains(m.Body, "host=web1") {
		t.Fatalf("message: %+v", m)
	}
	if f, p := v.e.Counts("acme"); f != 2 || p != 0 {
		t.Fatal(f, p)
	}
	// nothing more while it keeps firing
	for i := 0; i < 5; i++ {
		v.step(16 * time.Second)
	}
	if v.r.count() != 1 {
		t.Fatalf("a firing alert is not announced again every evaluation: %d", v.r.count())
	}
	// it calms down: two calm evaluations, then one resolved message
	v.q.set("acme", map[string]float64{"web1": 10, "web2": 10, "db1": 10})
	v.step(16 * time.Second)
	if v.r.count() != 1 || len(v.e.ListAlerts("acme")) != 2 {
		t.Fatal("one calm evaluation is not recovery")
	}
	v.step(16 * time.Second)
	if len(v.e.ListAlerts("acme")) != 0 {
		t.Fatal("two calm evaluations resolve")
	}
	v.step(31 * time.Second)
	if v.r.count() != 2 || v.r.last().Status != "resolved" || len(v.r.last().Alerts) != 2 || v.r.last().Title != "[RESOLVED] Disk almost full (2 alerts)" {
		t.Fatalf("one resolved message: %d %+v", v.r.count(), v.r.last())
	}
	v.step(time.Hour)
	if v.r.count() != 2 {
		t.Fatal("no further messages")
	}
	h := v.e.History("acme", 10)
	types := ""
	for _, ev := range h {
		types += ev.Type + ","
	}
	if strings.Count(types, "firing") != 2 || strings.Count(types, "resolved") != 2 {
		t.Fatalf("history records each alert's firing and resolved: %s", types)
	}
}

func TestForDurationDelaysTheAlert(t *testing.T) {
	v := newEnv(t)
	v.channel(t, "acme", "ops", ChannelIn{})
	r := diskRule()
	r.ForSec = 60
	v.rule(t, "acme", r)
	v.q.set("acme", map[string]float64{"web1": 95})
	v.step(0)
	if f, p := v.e.Counts("acme"); f != 0 || p != 1 {
		t.Fatalf("pending first: %d %d", f, p)
	}
	for i := 0; i < 3; i++ {
		v.step(16 * time.Second)
	}
	if v.r.count() != 0 {
		t.Fatal("not yet")
	}
	v.step(16 * time.Second) // 64 s since it started
	if f, _ := v.e.Counts("acme"); f != 1 {
		t.Fatal("now firing")
	}
	v.step(31 * time.Second)
	if v.r.count() != 1 {
		t.Fatalf("%d", v.r.count())
	}
	// a pending alert that calms down is never announced
	v2 := newEnv(t)
	v2.channel(t, "acme", "ops", ChannelIn{})
	v2.rule(t, "acme", r)
	v2.q.set("acme", map[string]float64{"web1": 95})
	v2.step(0)
	v2.q.set("acme", map[string]float64{"web1": 10})
	v2.step(16 * time.Second)
	v2.step(time.Hour)
	if v2.r.count() != 0 || len(v2.e.ListAlerts("acme")) != 0 {
		t.Fatal("a blip shorter than 'for' makes no noise")
	}
}

func TestRepeatAndAcknowledge(t *testing.T) {
	v := newEnv(t)
	v.channel(t, "acme", "ops", ChannelIn{})
	v.rule(t, "acme", diskRule())
	v.q.set("acme", map[string]float64{"web1": 95})
	v.step(0)
	v.step(31 * time.Second)
	if v.r.count() != 1 {
		t.Fatal("first")
	}
	for i := 0; i < 8; i++ { // 2 hours
		v.step(15 * time.Minute)
	}
	if v.r.count() != 1 {
		t.Fatalf("not repeated before 4 hours: %d", v.r.count())
	}
	for i := 0; i < 9; i++ { // 4.25 hours in total
		v.step(15 * time.Minute)
	}
	if v.r.count() != 2 || v.r.last().Status != "firing" {
		t.Fatalf("repeated after 4 hours: %d", v.r.count())
	}
	fp := v.e.ListAlerts("acme")[0].Fingerprint
	if err := v.e.Ack("acme", fp, "anna", true); err != nil {
		t.Fatal(err)
	}
	if a := v.e.ListAlerts("acme")[0]; !a.Acked || a.AckedBy != "anna" || a.State != "firing" {
		t.Fatalf("an acknowledged alert stays firing: %+v", a)
	}
	for i := 0; i < 40; i++ { // 10 hours
		v.step(15 * time.Minute)
	}
	if v.r.count() != 2 {
		t.Fatalf("acknowledging stops the repeats: %d", v.r.count())
	}
	if err := v.e.Ack("acme", fp, "anna", false); err != nil {
		t.Fatal(err)
	}
	v.step(15 * time.Minute)
	v.step(31 * time.Second) // the repeat waits in the group like any message
	if v.r.count() != 3 {
		t.Fatalf("removing the acknowledgement lets the repeats resume: %d", v.r.count())
	}
	if err := v.e.Ack("acme", "nope", "anna", true); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := v.e.Ack("globex", fp, "mallory", true); !errors.Is(err, ErrNotFound) {
		t.Fatal("another tenant cannot acknowledge")
	}
}

func TestSilence(t *testing.T) {
	v := newEnv(t)
	v.channel(t, "acme", "ops", ChannelIn{})
	v.rule(t, "acme", diskRule())
	if _, err := v.e.CreateSilence("acme", map[string]string{"host": "web1"}, time.Hour, "planned reboot", "anna"); err != nil {
		t.Fatal(err)
	}
	v.q.set("acme", map[string]float64{"web1": 95, "web2": 96})
	v.step(0)
	v.step(31 * time.Second)
	if v.r.count() != 1 || len(v.r.last().Alerts) != 1 || v.r.last().Alerts[0].Labels["host"] != "web2" {
		t.Fatalf("only web2 is announced: %+v", v.r.last())
	}
	al := v.e.ListAlerts("acme")
	silenced := 0
	for _, a := range al {
		if a.Silenced {
			silenced++
			if a.Labels["host"] != "web1" {
				t.Fatal("wrong alert silenced")
			}
		}
	}
	if len(al) != 2 || silenced != 1 {
		t.Fatalf("a silenced alert is still shown, marked as silenced: %+v", al)
	}
	if f, _ := v.e.Counts("acme"); f != 1 {
		t.Fatalf("silenced alerts are not counted as firing: %d", f)
	}
	// the silence ends while the alert still fires: it is announced now
	v.step(61 * time.Minute)
	v.step(31 * time.Second)
	if v.r.count() != 2 || v.r.last().Alerts[0].Labels["host"] != "web1" {
		t.Fatalf("after the silence ends the alert is announced: %d %+v", v.r.count(), v.r.last())
	}
	// validation
	for name, c := range map[string]struct {
		m map[string]string
		d time.Duration
	}{"no matcher": {nil, time.Hour}, "too short": {map[string]string{"a": "b"}, time.Second}, "too long": {map[string]string{"a": "b"}, 100 * 24 * time.Hour}, "bad label": {map[string]string{"bad key": "b"}, time.Hour}} {
		if _, err := v.e.CreateSilence("acme", c.m, c.d, "", "x"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a silence of another tenant does nothing here, and cannot be deleted from here
	s, _ := v.e.CreateSilence("globex", map[string]string{"host": "web2"}, time.Hour, "", "g")
	if err := v.e.DeleteSilence("acme", s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("tenant isolation")
	}
}

func TestRetriesWithBackoffThenGivesUpThenRepeats(t *testing.T) {
	v := newEnv(t)
	ch := v.channel(t, "acme", "ops", ChannelIn{})
	v.rule(t, "acme", diskRule())
	v.q.set("acme", map[string]float64{"web1": 95})
	v.r.fail = 2 // the destination is down for two attempts
	v.step(0)
	v.step(31 * time.Second) // attempt 1 fails
	if v.r.count() != 0 {
		t.Fatal("failed")
	}
	if h := v.e.ListChannels("acme")[0].Health; h.Error == "" || !h.LastOK.IsZero() {
		t.Fatalf("health shows the failure: %+v", h)
	}
	v.step(5 * time.Second) // too early to retry (10 s)
	if len(v.e.Deliveries("acme")) != 1 {
		t.Fatalf("no retry yet: %+v", v.e.Deliveries("acme"))
	}
	v.step(6 * time.Second) // attempt 2 fails
	v.step(31 * time.Second)
	if v.r.count() != 1 || v.r.last().Status != "firing" {
		t.Fatalf("attempt 3 succeeds after backing off: %d", v.r.count())
	}
	d := v.e.Deliveries("acme")
	if len(d) != 3 || d[0].OK != true || d[1].OK || d[2].Attempt != 1 || d[0].Attempt != 3 {
		t.Fatalf("delivery log: %+v", d)
	}
	if h := v.e.ListChannels("acme")[0].Health; h.Error != "" || h.LastOK.IsZero() {
		t.Fatalf("health recovers: %+v", h)
	}
	_ = ch
	// a destination that stays down: after the last retry Lumen gives up and says so
	v2 := newEnv(t)
	v2.channel(t, "acme", "ops", ChannelIn{})
	v2.rule(t, "acme", diskRule())
	v2.q.set("acme", map[string]float64{"web1": 95})
	v2.r.fail = 100
	v2.step(0)
	for i := 0; i < 6; i++ {
		v2.step(31 * time.Second)
	}
	d2 := v2.e.Deliveries("acme")
	if len(d2) != 3 || !d2[0].GaveUp || d2[0].Error == "" {
		t.Fatalf("it gives up after the retries, and the log says so: %+v", d2)
	}
	// the destination comes back: the alert is announced again at the next repeat
	v2.r.mu.Lock()
	v2.r.fail = 0
	v2.r.mu.Unlock()
	v2.step(5 * time.Hour)
	v2.step(31 * time.Second)
	if v2.r.count() != 1 {
		t.Fatalf("an alert that could not be delivered is announced again later: %d", v2.r.count())
	}
}

func TestRestartDoesNotRepeatOrLoseNotifications(t *testing.T) {
	db, _ := docstore.OpenFile(t.TempDir())
	clk := &clock{t: t0}
	a := newEnvOn(t, db, clk)
	a.channel(t, "acme", "ops", ChannelIn{})
	a.rule(t, "acme", diskRule())
	a.q.set("acme", map[string]float64{"web1": 95})
	a.step(0)
	a.step(31 * time.Second)
	if a.r.count() != 1 {
		t.Fatal("announced once")
	}
	// the server restarts: a new engine on the same store
	b := newEnvOn(t, db, clk)
	b.q.set("acme", map[string]float64{"web1": 95})
	b.step(16 * time.Second)
	b.step(60 * time.Second)
	if b.r.count() != 0 || len(b.e.ListAlerts("acme")) != 1 || b.e.ListAlerts("acme")[0].State != "firing" {
		t.Fatalf("after a restart an alert that was already announced is not announced again: %d", b.r.count())
	}
	// an alert that fired but was never delivered (the server stopped inside the group wait) is announced after the restart
	db2, _ := docstore.OpenFile(t.TempDir())
	clk2 := &clock{t: t0}
	c := newEnvOn(t, db2, clk2)
	c.channel(t, "acme", "ops", ChannelIn{})
	c.rule(t, "acme", diskRule())
	c.q.set("acme", map[string]float64{"web1": 95})
	c.step(0) // fires, waits in the group, never sent
	d := newEnvOn(t, db2, clk2)
	d.q.set("acme", map[string]float64{"web1": 95})
	d.step(16 * time.Second)
	d.step(40 * time.Second)
	if d.r.count() != 1 {
		t.Fatalf("an alert nobody heard about is announced after a restart: %d", d.r.count())
	}
}

func TestEvaluationErrorKeepsAlertsAndIsReported(t *testing.T) {
	v := newEnv(t)
	v.channel(t, "acme", "ops", ChannelIn{})
	r := v.rule(t, "acme", diskRule())
	v.q.set("acme", map[string]float64{"web1": 95})
	v.step(0)
	v.step(31 * time.Second)
	v.q.err = errors.New("clickhouse is down")
	for i := 0; i < 6; i++ {
		v.step(16 * time.Second)
	}
	al := v.e.ListAlerts("acme")
	if len(al) != 1 || al[0].State != "firing" || al[0].OKStreak != 0 {
		t.Fatalf("a failing database must not resolve alerts: %+v", al)
	}
	if h := v.e.RuleHealth("acme"); !strings.Contains(h[r.ID], "clickhouse is down") {
		t.Fatalf("the rule shows why it cannot be evaluated: %v", h)
	}
	v.q.err = nil
	v.step(16 * time.Second)
	if len(v.e.RuleHealth("acme")) != 0 {
		t.Fatal("health recovers")
	}
	if v.r.count() != 1 {
		t.Fatal("no messages from errors")
	}
}

func TestTenantIsolation(t *testing.T) {
	v := newEnv(t)
	v.channel(t, "acme", "acme-ops", ChannelIn{})
	other := v.channel(t, "globex", "globex-ops", ChannelIn{})
	ra := v.rule(t, "acme", diskRule())
	v.rule(t, "globex", diskRule())
	v.q.set("acme", map[string]float64{"web1": 95})
	v.q.set("globex", map[string]float64{"web1": 10})
	v.step(0)
	v.step(31 * time.Second)
	if len(v.e.ListAlerts("acme")) != 1 || len(v.e.ListAlerts("globex")) != 0 {
		t.Fatal("alerts are per tenant")
	}
	if v.r.count() != 1 || v.r.last().Tenant != "acme" || v.r.last().Channel != "acme-ops" {
		t.Fatalf("only acme's channel is used for acme's alert: %+v", v.r.last())
	}
	v.q.mu.Lock()
	seen := map[string]bool{}
	for _, tn := range v.q.tenants {
		seen[tn] = true
	}
	v.q.mu.Unlock()
	if !seen["acme"] || !seen["globex"] {
		t.Fatal("every rule is evaluated with its own tenant")
	}
	if len(v.e.ListRules("globex")) != 1 || len(v.e.ListChannels("globex")) != 1 || len(v.e.ListChannels("acme")) != 1 {
		t.Fatal("lists are per tenant")
	}
	if _, ok := v.e.GetRule("globex", ra.ID); ok {
		t.Fatal("another tenant cannot read a rule")
	}
	if _, err := v.e.PutRule("globex", ra.ID, diskRule()); !errors.Is(err, ErrNotFound) {
		t.Fatal("another tenant cannot change a rule")
	}
	if err := v.e.DeleteRule("globex", ra.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("another tenant cannot delete a rule")
	}
	if _, err := v.e.PutChannel("acme", other.ID, ChannelIn{Name: "x", Type: "rec", Settings: map[string]string{"target": "x"}}); !errors.Is(err, ErrNotFound) {
		t.Fatal("another tenant cannot change a channel")
	}
	if err := v.e.TestChannel(context.Background(), "acme", other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("another tenant cannot send through a channel")
	}
	if err := v.e.DeleteChannel("acme", other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("another tenant cannot delete a channel")
	}
	if len(v.e.History("globex", 10)) != 0 || len(v.e.Deliveries("globex")) != 0 {
		t.Fatal("history and the delivery log are per tenant")
	}
}

func TestDeletingOrDisablingARuleClearsItsAlerts(t *testing.T) {
	v := newEnv(t)
	v.channel(t, "acme", "ops", ChannelIn{})
	r := v.rule(t, "acme", diskRule())
	v.q.set("acme", map[string]float64{"web1": 95})
	v.step(0)
	r.Enabled = false
	if _, err := v.e.PutRule("acme", r.ID, r); err != nil {
		t.Fatal(err)
	}
	if len(v.e.ListAlerts("acme")) != 0 {
		t.Fatal("a disabled rule has no alerts")
	}
	v.step(time.Hour)
	if v.r.count() != 0 {
		t.Fatal("and sends nothing, not even what was waiting in the group")
	}
	r.Enabled = true
	v.e.PutRule("acme", r.ID, r)
	v.step(0)
	if len(v.e.ListAlerts("acme")) != 1 {
		t.Fatal("enabled again")
	}
	if err := v.e.DeleteRule("acme", r.ID); err != nil || len(v.e.ListAlerts("acme")) != 0 || len(v.e.ListRules("acme")) != 0 {
		t.Fatal("deleting removes the alerts too")
	}
	if _, err := v.e.PutRule("acme", "", Rule{Name: "bad", Kind: "weird"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid rules are refused")
	}
	for i := 0; i < MaxRulesPerTenant; i++ {
		if _, err := v.e.PutRule("acme", "", diskRule()); err != nil {
			if i < MaxRulesPerTenant {
				t.Fatalf("rule %d: %v", i, err)
			}
		}
	}
	if _, err := v.e.PutRule("acme", "", diskRule()); !errors.Is(err, ErrInvalid) {
		t.Fatal("a tenant cannot have unlimited rules")
	}
}

func TestChannelSecretsAreSealedAndNeverReturned(t *testing.T) {
	v := newEnv(t)
	out, err := v.e.PutChannel("acme", "", ChannelIn{Name: "ops", Type: "rec", Enabled: true, Settings: map[string]string{"target": "x"}, Secrets: map[string]string{"token": "TOP-SECRET-TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "TOP-SECRET") || !out.HasSecret["token"] {
		t.Fatalf("the answer says a secret is set but never shows it: %s", b)
	}
	c, cancel := ctx10()
	defer cancel()
	d, _ := v.db.Get(c, collChannels, out.ID)
	if strings.Contains(string(d.Data), "TOP-SECRET") {
		t.Fatalf("the secret must be encrypted in the store: %s", d.Data)
	}
	// the notifier gets the real value
	if err := v.e.TestChannel(context.Background(), "acme", out.ID); err != nil {
		t.Fatal(err)
	}
	if v.r.seen[0].Secrets["token"] != "TOP-SECRET-TOKEN" {
		t.Fatal("the notifier must receive the decrypted secret")
	}
	// an update without the secret keeps it; the secret can also be removed
	out2, err := v.e.PutChannel("acme", out.ID, ChannelIn{Name: "ops renamed", Type: "rec", Enabled: true, Settings: map[string]string{"target": "y"}})
	if err != nil || !out2.HasSecret["token"] || out2.Name != "ops renamed" {
		t.Fatalf("%+v %v", out2, err)
	}
	v.e.TestChannel(context.Background(), "acme", out.ID)
	if v.r.seen[1].Secrets["token"] != "TOP-SECRET-TOKEN" || v.r.seen[1].Settings["target"] != "y" {
		t.Fatal("kept")
	}
	out3, _ := v.e.PutChannel("acme", out.ID, ChannelIn{Name: "ops", Type: "rec", Enabled: true, Settings: map[string]string{"target": "y"}, ClearSecrets: []string{"token"}})
	if out3.HasSecret["token"] {
		t.Fatal("cleared")
	}
	// validation
	for name, in := range map[string]ChannelIn{
		"unknown type":      {Name: "x", Type: "carrier-pigeon"},
		"missing target":    {Name: "x", Type: "rec", Settings: map[string]string{}},
		"bad target":        {Name: "x", Type: "rec", Settings: map[string]string{"target": "bad"}},
		"unknown setting":   {Name: "x", Type: "rec", Settings: map[string]string{"target": "x", "evil": "1"}},
		"secret as setting": {Name: "x", Type: "rec", Settings: map[string]string{"target": "x", "token": "t"}},
		"unknown secret":    {Name: "x", Type: "rec", Settings: map[string]string{"target": "x"}, Secrets: map[string]string{"nope": "1"}},
		"bad severity":      {Name: "x", Type: "rec", Settings: map[string]string{"target": "x"}, Severities: []string{"urgent"}},
		"bad name":          {Name: "a\nb", Type: "rec", Settings: map[string]string{"target": "x"}},
		"control chars":     {Name: "x", Type: "rec", Settings: map[string]string{"target": "x\ny"}},
	} {
		if _, err := v.e.PutChannel("acme", "", in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s must be refused: %v", name, err)
		}
	}
	if _, err := v.e.PutChannel("acme", out.ID, ChannelIn{Name: "x", Type: "webhook", Settings: map[string]string{"url": "https://x.example.com"}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("the type cannot change")
	}
	// the stored secret cannot be decrypted with another key: the channel says so instead of failing silently
	box2, _ := secretbox.New("another key")
	v2 := &Engine{DB: v.db, Box: box2, Eval: v.e.Eval, Reg: v.e.Reg, Now: v.clk.Now}
	v.e.PutChannel("acme", out.ID, ChannelIn{Name: "ops", Type: "rec", Enabled: true, Settings: map[string]string{"target": "y"}, Secrets: map[string]string{"token": "again"}})
	v2.Load()
	if err := v2.TestChannel(context.Background(), "acme", out.ID); err == nil || !strings.Contains(err.Error(), "LUMEN_SECRET_KEY") {
		t.Fatalf("%v", err)
	}
	for i := 0; i < MaxChannelsPerTenant; i++ {
		v.e.PutChannel("globex", "", ChannelIn{Name: fmt.Sprint("c", i), Type: "rec", Settings: map[string]string{"target": "x"}})
	}
	if _, err := v.e.PutChannel("globex", "", ChannelIn{Name: "one more", Type: "rec", Settings: map[string]string{"target": "x"}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("limit")
	}
}

func TestChannelRoutingBySeverityAndLabels(t *testing.T) {
	v := newEnv(t)
	v.channel(t, "acme", "criticals", ChannelIn{Severities: []string{"critical"}})
	v.channel(t, "acme", "web1 only", ChannelIn{Match: map[string]string{"host": "web1"}})
	v.channel(t, "acme", "warnings", ChannelIn{Severities: []string{"warning"}})
	v.rule(t, "acme", diskRule()) // critical
	v.q.set("acme", map[string]float64{"web1": 95, "web2": 96})
	v.step(0)
	v.step(31 * time.Second)
	got := map[string]int{}
	v.r.mu.Lock()
	for _, m := range v.r.msgs {
		got[m.Channel] = len(m.Alerts)
	}
	v.r.mu.Unlock()
	if got["criticals"] != 2 || got["web1 only"] != 1 || got["warnings"] != 0 || len(got) != 2 {
		t.Fatalf("each channel gets what matches it: %v", got)
	}
}

func TestExternalReferencesComeBackOnResolve(t *testing.T) {
	v := newEnv(t)
	v.r.refs = true // the notifier creates a ticket per alert
	ch := v.channel(t, "acme", "tickets", ChannelIn{})
	v.rule(t, "acme", diskRule())
	v.q.set("acme", map[string]float64{"web1": 95})
	v.step(0)
	v.step(31 * time.Second)
	if a := v.e.ListAlerts("acme")[0]; a.Refs[ch.ID] != "TICKET-web1" {
		t.Fatalf("the engine remembers which ticket belongs to the alert: %+v", a.Refs)
	}
	v.q.set("acme", map[string]float64{"web1": 10})
	v.step(16 * time.Second)
	v.step(16 * time.Second)
	v.step(31 * time.Second)
	m := v.r.last()
	if m.Status != "resolved" || m.Alerts[0].Ref != "TICKET-web1" {
		t.Fatalf("the resolve message carries the ticket so it can be closed: %+v", m)
	}
}

func TestTestChannelAndHeartbeat(t *testing.T) {
	v := newEnv(t)
	ch := v.channel(t, "acme", "ops", ChannelIn{})
	if err := v.e.TestChannel(context.Background(), "acme", ch.ID); err != nil {
		t.Fatal(err)
	}
	if m := v.r.last(); !strings.HasPrefix(m.Title, "[TEST]") || len(m.Alerts) != 1 {
		t.Fatalf("%+v", m)
	}
	v.r.mu.Lock()
	v.r.fail = 1
	v.r.mu.Unlock()
	if err := v.e.TestChannel(context.Background(), "acme", ch.ID); err == nil || !strings.Contains(err.Error(), "down") {
		t.Fatal("a failing test says why")
	}
	if d := v.e.Deliveries("acme"); len(d) != 2 || d[0].Status != "test" || d[0].OK || !d[1].OK {
		t.Fatalf("%+v", d)
	}
	// heartbeat through the real registry and the real notifier
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer srv.Close()
	db, _ := docstore.OpenFile(t.TempDir())
	box, _ := secretbox.New("k")
	clk := &clock{t: t0}
	e := &Engine{DB: db, Box: box, Eval: &Evaluator{}, Now: clk.Now, Guard: Guard{AllowPrivate: true}}
	if _, err := e.PutChannel("acme", "", ChannelIn{Name: "hb", Type: "heartbeat", Enabled: true, Settings: map[string]string{"interval_seconds": "30"}, Secrets: map[string]string{"url": srv.URL}}); err != nil {
		t.Fatal(err)
	}
	e.Dispatch(context.Background())
	if calls != 1 {
		t.Fatalf("the first heartbeat goes out at once: %d", calls)
	}
	clk.add(20 * time.Second)
	e.Dispatch(context.Background())
	if calls != 1 {
		t.Fatal("not before the interval")
	}
	clk.add(15 * time.Second)
	e.Dispatch(context.Background())
	if calls != 2 {
		t.Fatalf("every 30 seconds: %d", calls)
	}
	if h := e.ListChannels("acme")[0].Health; h.LastOK.IsZero() {
		t.Fatal("health")
	}
}

func TestPruneHistoryAndTemplates(t *testing.T) {
	v := newEnv(t)
	for i := 0; i < 30; i++ {
		v.e.history("firing", Alert{Tenant: "acme", RuleName: "r", Severity: "info", Labels: map[string]string{"i": fmt.Sprint(i)}}, "")
		v.clk.add(time.Second)
	}
	v.e.Prune(10)
	h := v.e.History("acme", 100)
	if len(h) != 10 || h[0].Labels["i"] != "29" {
		t.Fatalf("the newest are kept: %d %v", len(h), h[0].Labels)
	}
	seen := map[string]bool{}
	for _, tpl := range Templates() {
		if seen[tpl.Name] {
			t.Fatal("duplicate template")
		}
		seen[tpl.Name] = true
		c := tpl
		if err := c.Normalize(); err != nil {
			t.Errorf("template %q is not a valid rule: %v", tpl.Name, err)
		}
	}
	if len(seen) < 8 {
		t.Fatal("starter rules")
	}
}

type entNotifier struct{ recNotifier }

func (entNotifier) Type() string    { return "ent" }
func (entNotifier) Label() string   { return "Enterprise recorder" }
func (entNotifier) Edition() string { return "enterprise" }

type fakeLic struct{ on bool }

func (f *fakeLic) Allows(ed string) bool { return ed == "community" || f.on }

func TestEnterpriseChannelsNeedALicence(t *testing.T) {
	v := newEnv(t)
	v.e.Reg.Register(entNotifier{recNotifier{v.r}})
	lic := &fakeLic{}
	v.e.License = lic
	ent := ChannelIn{Name: "tickets", Type: "ent", Enabled: true, Settings: map[string]string{"target": "x"}}
	// without a licence an Enterprise channel cannot be created, and the reason says what to do
	if _, err := v.e.PutChannel("acme", "", ent); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "needs a Lumen Enterprise licence") || !strings.Contains(err.Error(), "Settings") {
		t.Fatalf("%v", err)
	}
	if _, err := v.e.PutChannel("acme", "", ChannelIn{Name: "ok", Type: "rec", Enabled: true, Settings: map[string]string{"target": "x"}}); err != nil {
		t.Fatalf("Community channels never need a licence: %v", err)
	}
	if v.e.Allowed(entNotifier{}) || !v.e.Allowed(recNotifier{}) {
		t.Fatal("Allowed")
	}
	e2 := &Engine{}
	if e2.Allowed(entNotifier{}) {
		t.Fatal("an engine without a licence manager is Community only")
	}
	// with a licence both work
	lic.on = true
	entCh, err := v.e.PutChannel("acme", "", ent)
	if err != nil || entCh.Edition != "enterprise" || entCh.Locked {
		t.Fatalf("%+v %v", entCh, err)
	}
	v.rule(t, "acme", diskRule())
	v.q.set("acme", map[string]float64{"web1": 95})
	v.step(0)
	v.step(31 * time.Second)
	if v.r.count() != 2 {
		t.Fatalf("both channels get the alert: %d", v.r.count())
	}
	// the licence ends: a new alert reaches the Community channel only, and the Enterprise channel says why
	lic.on = false
	v.q.set("acme", map[string]float64{"web1": 95, "web2": 96})
	v.step(16 * time.Second)
	v.step(31 * time.Second)
	got := map[string]int{}
	v.r.mu.Lock()
	for _, m := range v.r.msgs {
		got[m.Channel]++
	}
	v.r.mu.Unlock()
	if got["ok"] != 2 || got["tickets"] != 1 {
		t.Fatalf("the Enterprise channel stops while the Community one keeps working: %v", got)
	}
	var tickets ChannelOut
	for _, c := range v.e.ListChannels("acme") {
		if c.Name == "tickets" {
			tickets = c
		}
	}
	if !tickets.Locked || !strings.Contains(tickets.Health.Error, "needs a Lumen Enterprise licence") {
		t.Fatalf("the channel is marked and says why: %+v", tickets)
	}
	if err := v.e.TestChannel(context.Background(), "acme", entCh.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a locked channel cannot be tested: %v", err)
	}
	if _, err := v.e.PutChannel("acme", entCh.ID, ent); !errors.Is(err, ErrInvalid) {
		t.Fatal("nor edited")
	}
	if err := v.e.DeleteChannel("acme", entCh.ID); err != nil {
		t.Fatal("but it can always be deleted")
	}
	// a message that was already waiting when the licence ended is not sent, and is not retried either
	lic.on = true
	ch2, _ := v.e.PutChannel("globex", "", ChannelIn{Name: "late", Type: "ent", Enabled: true, Settings: map[string]string{"target": "x"}})
	v.rule(t, "globex", diskRule())
	v.q.set("globex", map[string]float64{"web1": 99})
	v.step(0) // fires, waits in the group
	lic.on = false
	n := v.r.count()
	v.step(31 * time.Second)
	d := v.e.Deliveries("globex")
	if v.r.count() != n || len(d) != 1 || !d[0].GaveUp || !strings.Contains(d[0].Error, "licence") {
		t.Fatalf("a delivery that is refused for lack of a licence is not retried: %+v", d)
	}
	// renewed: it works again without anyone recreating the channel, and it is told about what is firing right now
	lic.on = true
	v.step(16 * time.Second)
	v.step(31 * time.Second)
	if v.r.count() != n+1 || v.r.last().Channel != "late" || v.r.last().Status != "firing" {
		t.Fatalf("after a renewal the channel hears about the alerts that fired while it was shut out, at once and not at the next reminder hours later: %d %+v", v.r.count(), v.r.last().Channel)
	}
	if c := v.e.ListChannels("globex")[0]; c.Locked || c.ID != ch2.ID {
		t.Fatalf("%+v", c)
	}
	if err := v.e.TestChannel(context.Background(), "globex", ch2.ID); err != nil {
		t.Fatalf("after renewal: %v", err)
	}
}
