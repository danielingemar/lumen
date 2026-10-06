package alerts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/model"
	"github.com/danielingemar/lumen/internal/registry"
	"github.com/danielingemar/lumen/internal/status"
)

var t0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func ruleFor(forSec int) Rule {
	return Rule{ID: "r1", Tenant: "acme", Name: "Disk almost full", Kind: KindMetric, Severity: "warning", ForSec: forSec, NoData: "ok", Labels: map[string]string{"team": "ops"}}
}

func fire(l map[string]string, v float64) Sample { return Sample{Labels: l, Value: v, Firing: true} }
func calm(l map[string]string, v float64) Sample { return Sample{Labels: l, Value: v, Firing: false} }

func types(tr []Transition) string {
	s := ""
	for _, t := range tr {
		s += t.Type + ";"
	}
	return s
}

func TestPendingFiringResolved(t *testing.T) {
	r := ruleFor(120) // must hold for 2 minutes
	l := map[string]string{"host": "web1"}
	st, tr := Step(r, nil, []Sample{fire(l, 95)}, false, t0, 2)
	fp := Fingerprint("r1", l)
	if len(tr) != 0 || st[fp].State != "pending" {
		t.Fatalf("condition just became true: pending, no message yet: %v %+v", types(tr), st[fp])
	}
	st, tr = Step(r, st, []Sample{fire(l, 96)}, false, t0.Add(60*time.Second), 2)
	if len(tr) != 0 || st[fp].State != "pending" {
		t.Fatalf("after 1 minute still pending: %v", types(tr))
	}
	st, tr = Step(r, st, []Sample{fire(l, 97)}, false, t0.Add(120*time.Second), 2)
	if types(tr) != "firing;" || st[fp].State != "firing" || st[fp].FiringSince != t0.Add(120*time.Second) || st[fp].Value != 97 {
		t.Fatalf("held for 2 minutes: fires once: %v %+v", types(tr), st[fp])
	}
	if a := tr[0].Alert; a.Labels["host"] != "web1" || a.Labels["team"] != "ops" || a.RuleName != "Disk almost full" {
		t.Fatalf("the alert carries the series labels and the rule's labels: %+v", a.Labels)
	}
	st, tr = Step(r, st, []Sample{fire(l, 97)}, false, t0.Add(180*time.Second), 2)
	if len(tr) != 0 {
		t.Fatal("a firing alert that keeps firing sends nothing new (repeats are the engine's job)")
	}
	// recovery needs two calm evaluations in a row
	st, tr = Step(r, st, []Sample{calm(l, 50)}, false, t0.Add(240*time.Second), 2)
	if len(tr) != 0 || st[fp].State != "firing" || st[fp].OKStreak != 1 {
		t.Fatalf("one calm evaluation is not recovery: %v %+v", types(tr), st[fp])
	}
	st, tr = Step(r, st, []Sample{calm(l, 50)}, false, t0.Add(300*time.Second), 2)
	if types(tr) != "resolved;" || len(st) != 0 {
		t.Fatalf("two calm evaluations resolve it: %v %v", types(tr), st)
	}
}

func TestNoFlappingAroundTheThreshold(t *testing.T) {
	r := ruleFor(0)
	l := map[string]string{"host": "web1"}
	st, tr := Step(r, nil, []Sample{fire(l, 91)}, false, t0, 2)
	if types(tr) != "firing;" {
		t.Fatalf("for=0 fires at once: %v", types(tr))
	}
	total := 1
	for i := 1; i <= 10; i++ { // alternates around the threshold
		s := fire(l, 91)
		if i%2 == 1 {
			s = calm(l, 89)
		}
		var tr []Transition
		st, tr = Step(r, st, []Sample{s}, false, t0.Add(time.Duration(i)*time.Minute), 2)
		total += len(tr)
	}
	if total != 1 {
		t.Fatalf("a value that jumps over and under the threshold must not send a message each time, got %d", total)
	}
}

func TestPendingThatCalmsDownIsForgottenSilently(t *testing.T) {
	r := ruleFor(300)
	l := map[string]string{"host": "web1"}
	st, _ := Step(r, nil, []Sample{fire(l, 95)}, false, t0, 2)
	st, tr := Step(r, st, []Sample{calm(l, 40)}, false, t0.Add(time.Minute), 2)
	if len(tr) != 0 || len(st) != 0 {
		t.Fatalf("never fired, nobody was told: nothing to resolve: %v %v", types(tr), st)
	}
}

func TestSeriesThatDisappearsResolves(t *testing.T) {
	r := ruleFor(0)
	a, b := map[string]string{"host": "a"}, map[string]string{"host": "b"}
	st, _ := Step(r, nil, []Sample{fire(a, 1), fire(b, 1)}, false, t0, 2)
	st, tr := Step(r, st, []Sample{fire(a, 1)}, false, t0.Add(time.Minute), 2) // b is gone
	st, tr2 := Step(r, st, []Sample{fire(a, 1)}, false, t0.Add(2*time.Minute), 2)
	if len(tr) != 0 || types(tr2) != "resolved;" || tr2[0].Alert.Labels["host"] != "b" || len(st) != 1 {
		t.Fatalf("an alert whose series is gone resolves after the recovery period: %v %v", types(tr), types(tr2))
	}
}

func TestNoDataPolicies(t *testing.T) {
	l := map[string]string{"host": "web1"}
	for _, c := range []struct {
		policy string
		want   string // transitions in the first no-data evaluation
	}{{"ok", ""}, {"keep", ""}, {"alert", "firing;"}} {
		r := ruleFor(0)
		r.NoData = c.policy
		st, _ := Step(r, nil, []Sample{fire(l, 95)}, false, t0, 2) // an alert is firing
		st2, tr := Step(r, st, nil, true, t0.Add(time.Minute), 2)
		if types(tr) != c.want {
			t.Errorf("%s: %q want %q", c.policy, types(tr), c.want)
		}
		switch c.policy {
		case "keep":
			if len(st2) != 1 || st2[Fingerprint("r1", l)].OKStreak != 0 {
				t.Errorf("keep leaves the state exactly as it was: %+v", st2)
			}
		case "alert":
			if _, ok := st2[Fingerprint("r1", map[string]string{"nodata": "true"})]; !ok {
				t.Errorf("alert raises a no-data alert: %+v", st2)
			}
		}
	}
}

func TestNormalizeAndHelpers(t *testing.T) {
	ok := Rule{Name: "x", Kind: KindMetric, Metric: "system.cpu.utilization", Op: ">", Threshold: 0.9}
	if err := ok.Normalize(); err != nil || ok.IntervalSec != 60 || ok.WindowSec != 300 || ok.Reduce != "avg" || ok.Severity != "warning" || ok.NoData != "ok" {
		t.Fatalf("defaults: %+v %v", ok, err)
	}
	for name, r := range map[string]Rule{
		"no name":      {Kind: KindMetric, Metric: "m", Op: ">"},
		"bad kind":     {Name: "x", Kind: "weird"},
		"bad op":       {Name: "x", Kind: KindMetric, Metric: "m", Op: "=~"},
		"no metric":    {Name: "x", Kind: KindMetric, Op: ">"},
		"fast":         {Name: "x", Kind: KindMetric, Metric: "m", Op: ">", IntervalSec: 5},
		"long name":    {Name: string(make([]byte, 90)), Kind: KindMetric, Metric: "m", Op: ">"},
		"newline":      {Name: "a\nb", Kind: KindMetric, Metric: "m", Op: ">"},
		"bad status":   {Name: "x", Kind: KindStatus, Status: "everything"},
		"bad label":    {Name: "x", Kind: KindMetric, Metric: "m", Op: ">", Labels: map[string]string{"bad key": "v"}},
		"bad severity": {Name: "x", Kind: KindMetric, Metric: "m", Op: ">", Severity: "urgent"},
		"short window": {Name: "x", Kind: KindMetric, Metric: "m", Op: ">", WindowSec: 10},
	} {
		if err := r.Normalize(); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
	if v, ok := Reduce([][2]float64{{0, 1}, {0, 5}, {0, 3}}, "max"); !ok || v != 5 {
		t.Fatal(v)
	}
	if v, _ := Reduce([][2]float64{{0, 1}, {0, 5}, {0, 3}}, "avg"); v != 3 {
		t.Fatal(v)
	}
	if v, _ := Reduce([][2]float64{{0, 1}, {0, 5}, {0, 3}}, "last"); v != 3 {
		t.Fatal(v)
	}
	if _, ok := Reduce(nil, "avg"); ok {
		t.Fatal("empty has no value")
	}
	if Fingerprint("r", map[string]string{"a": "1", "b": "2"}) != Fingerprint("r", map[string]string{"b": "2", "a": "1"}) || Fingerprint("r", nil) == Fingerprint("s", nil) {
		t.Fatal("fingerprints are stable and per rule")
	}
	if !Matches(map[string]string{"host": "web1", "severity": "*"}, map[string]string{"host": "web1", "severity": "warning"}) || Matches(map[string]string{"host": "web1"}, map[string]string{"host": "web2"}) || Matches(nil, map[string]string{"host": "web1"}) {
		t.Fatal("matchers: all must match, * matches any present value, an empty matcher set matches nothing")
	}
}

type fakeQ struct {
	series []model.Series
	err    error
	got    []model.SeriesQuery
	tenant []string
}

func (f *fakeQ) Series(_ context.Context, tenant string, q model.SeriesQuery) ([]model.Series, error) {
	f.got = append(f.got, q)
	f.tenant = append(f.tenant, tenant)
	return f.series, f.err
}

type fakeSt struct{ s status.Summary }

func (f fakeSt) Summary(context.Context, string) (status.Summary, error) { return f.s, nil }

func pts(vs ...float64) [][2]float64 {
	var o [][2]float64
	for i, v := range vs {
		o = append(o, [2]float64{float64(t0.Add(time.Duration(i) * time.Minute).UnixMilli()), v})
	}
	return o
}

func TestEvaluateMetricLogAndStatus(t *testing.T) {
	q := &fakeQ{series: []model.Series{{Labels: map[string]string{"mountpoint": "/"}, Points: pts(0.5, 0.95, 0.97)}, {Labels: map[string]string{"mountpoint": "/data"}, Points: pts(0.2, 0.3, 0.2)}}}
	ev := &Evaluator{Q: q}
	r := Rule{ID: "r", Tenant: "acme", Kind: KindMetric, Metric: "system.filesystem.utilization", Reduce: "max", Op: ">", Threshold: 0.9, WindowSec: 300, GroupBy: "mountpoint", Filters: []Filter{{"host", "web1"}}}
	s, noData, err := ev.Eval(context.Background(), r, t0.Add(5*time.Minute))
	if err != nil || noData || len(s) != 2 || !s[0].Firing || s[0].Value != 0.97 || s[1].Firing {
		t.Fatalf("metric: %+v %v %v", s, noData, err)
	}
	g := q.got[0]
	if q.tenant[0] != "acme" || g.Source != "metric" || g.Name != "system.filesystem.utilization" || g.Agg != "max" || g.GroupBy != "mountpoint" || g.Filters["host"] != "web1" || g.StepSec != 60 || g.To.Sub(g.From) != 5*time.Minute {
		t.Fatalf("the query carries the tenant, the metric, the filters and exactly the window: %+v tenant %v", g, q.tenant)
	}
	// avg over the window, and sum for log counts
	r2 := Rule{ID: "r2", Kind: KindMetric, Metric: "m", Reduce: "avg", Op: ">", Threshold: 0.6, WindowSec: 300}
	q.series = []model.Series{{Labels: map[string]string{}, Points: pts(0.5, 0.95, 0.9)}}
	if s, _, _ := ev.Eval(context.Background(), r2, t0); len(s) != 1 || !s[0].Firing {
		t.Fatalf("avg of 0.5, 0.95, 0.9 is above 0.6: %+v", s)
	}
	q.series = nil
	if _, noData, _ := ev.Eval(context.Background(), r2, t0); !noData {
		t.Fatal("no series at all is no-data")
	}
	rl := Rule{ID: "rl", Tenant: "acme", Kind: KindLog, Service: "nginx", LogSeverity: "ERROR", Op: ">", Threshold: 20, WindowSec: 300}
	q.series = []model.Series{{Labels: map[string]string{}, Points: pts(5, 10, 8)}}
	if s, nd, _ := ev.Eval(context.Background(), rl, t0); nd || len(s) != 1 || s[0].Value != 23 || !s[0].Firing {
		t.Fatalf("23 error lines in the window: %+v", s)
	}
	q.series = nil
	if s, nd, _ := ev.Eval(context.Background(), rl, t0); nd || len(s) != 1 || s[0].Value != 0 || s[0].Firing {
		t.Fatalf("no log lines is a count of zero, not no-data: %+v %v", s, nd)
	}
	if lq := q.got[len(q.got)-1]; lq.Source != "logs" || lq.Service != "nginx" || lq.Severity != "ERROR" {
		t.Fatalf("log query: %+v", lq)
	}
	q.err = errors.New("clickhouse is down")
	if _, _, err := ev.Eval(context.Background(), r2, t0); err == nil {
		t.Fatal("a failing query is an error, not 'no data'")
	}
	// status
	sum := status.Summary{
		HostList:  []status.Host{{Name: "web1", Status: "up"}, {Name: "old1", Status: "down"}, {Name: "new1", Status: "pending"}},
		Instances: []status.Instance{{InstanceOut: registry.InstanceOut{Name: "ks"}, Status: "down"}, {InstanceOut: registry.InstanceOut{Name: "ok"}, Status: "up"}},
		Down:      []status.Item{{Kind: "service", Name: "postfix", Host: "web2"}, {Kind: "container", Name: "db", Host: "web2"}},
	}
	se := &Evaluator{St: fakeSt{sum}}
	for kind, want := range map[string]string{"host_down": "old1", "instance_down": "ks", "service_down": "postfix", "container_down": "db"} {
		s, nd, err := se.Eval(context.Background(), Rule{Kind: KindStatus, Status: kind}, t0)
		if err != nil || nd || len(s) != 1 || !s[0].Firing {
			t.Fatalf("%s: %+v", kind, s)
		}
		found := false
		for _, v := range s[0].Labels {
			found = found || v == want
		}
		if !found {
			t.Errorf("%s: %+v should mention %s (pending hosts and up instances are not down)", kind, s[0].Labels, want)
		}
	}
}

func TestBacktest(t *testing.T) {
	// 10 one-minute points; the value is above 90 from minute 3 to minute 6, then falls
	q := &fakeQ{series: []model.Series{{Labels: map[string]string{"host": "web1"}, Points: pts(10, 20, 30, 95, 96, 97, 98, 20, 10, 10)}}}
	ev := &Evaluator{Q: q}
	r := Rule{ID: "r", Kind: KindMetric, Metric: "m", Reduce: "last", Op: ">", Threshold: 90, WindowSec: 60, ForSec: 120}
	iv, err := ev.Backtest(context.Background(), r, t0, t0.Add(10*time.Minute))
	if err != nil || len(iv) != 1 {
		t.Fatalf("one period: %+v %v", iv, err)
	}
	if !iv[0].Start.Equal(t0.Add(5*time.Minute)) || iv[0].Peak != 98 || iv[0].Labels["host"] != "web1" {
		t.Fatalf("it would have fired 2 minutes (the 'for' time) after the value went over: %+v", iv[0])
	}
	r.ForSec = 600
	if iv, _ := ev.Backtest(context.Background(), r, t0, t0.Add(10*time.Minute)); len(iv) != 0 {
		t.Fatalf("a condition that never lasts 10 minutes never fires: %+v", iv)
	}
	if _, err := ev.Backtest(context.Background(), Rule{Kind: KindStatus, Status: "host_down"}, t0, t0); err == nil {
		t.Fatal("status rules cannot be replayed")
	}
	// the average over a 3-minute window delays and smooths the start
	r2 := Rule{ID: "r", Kind: KindMetric, Metric: "m", Reduce: "avg", Op: ">", Threshold: 50, WindowSec: 180}
	iv, _ = ev.Backtest(context.Background(), r2, t0, t0.Add(10*time.Minute))
	if len(iv) != 1 || !iv[0].Start.After(t0.Add(3*time.Minute)) {
		t.Fatalf("an average needs a few high points before it crosses: %+v", iv)
	}
}
