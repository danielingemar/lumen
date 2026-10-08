package alerts

import (
	"context"
	"github.com/danielingemar/lumen/internal/health"
	"strings"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/model"
	"github.com/danielingemar/lumen/internal/status"
)

type captureQ struct{ last model.SeriesQuery }

func (c *captureQ) Series(_ context.Context, _ string, q model.SeriesQuery) ([]model.Series, error) {
	c.last = q
	return []model.Series{{Labels: map[string]string{}, Points: [][2]float64{{1, 5}}}}, nil
}

type fixedStatus struct{ s status.Summary }

func (f fixedStatus) Summary(context.Context, string) (status.Summary, error) { return f.s, nil }

type groups map[string][]string

func (g groups) GroupHosts(tenant, group string) []string { return g[tenant+"/"+group] }

func TestRulesCanBeLimitedToAHostGroup(t *testing.T) {
	q := &captureQ{}
	sum := status.Summary{
		HostList:  []status.Host{{Name: "web1", Status: "down"}, {Name: "db1", Status: "down"}, {Name: "web2", Status: "up"}},
		Down:      []status.Item{{Kind: "service", Name: "nginx", Host: "web1"}, {Kind: "service", Name: "postgres", Host: "db1"}, {Kind: "container", Name: "app", Host: "web1"}},
		Instances: []status.Instance{{Status: "down"}, {Status: "down"}},
	}
	sum.Instances[0].Name, sum.Instances[0].Host = "ks", "web1"
	sum.Instances[1].Name, sum.Instances[1].Host = "other", "db1"
	ev := &Evaluator{Q: q, St: fixedStatus{sum}, G: groups{"acme/web": {"web1", "web2"}, "acme/empty": nil}}
	now := time.Now()
	hostsOf := func(r Rule) string {
		r.Tenant = "acme"
		if _, _, err := ev.Eval(context.Background(), r, now); err != nil {
			t.Fatal(err)
		}
		return strings.Join(q.last.Hosts, ",")
	}
	// a metric rule and a log rule see only the hosts of the group
	m := Rule{Kind: KindMetric, Metric: "cpu", Reduce: "avg", WindowSec: 300, Op: ">", Threshold: 1}
	if got := hostsOf(m); got != "" {
		t.Fatalf("a rule without a group is not limited: %q", got)
	}
	m.Group = "web"
	if got := hostsOf(m); got != "web1,web2" {
		t.Fatalf("a metric rule is limited to the group's hosts: %q", got)
	}
	l := Rule{Kind: KindLog, Group: "web", WindowSec: 300, Op: ">", Threshold: 1}
	if got := hostsOf(l); got != "web1,web2" {
		t.Fatalf("%q", got)
	}
	// a group with no hosts, or that does not exist, sees nothing: it must not turn into "all hosts"
	for _, name := range []string{"empty", "no-such-group"} {
		m.Group = name
		if got := hostsOf(m); got != noHost {
			t.Fatalf("%s: %q", name, got)
		}
	}
	if (&Evaluator{Q: q, St: fixedStatus{sum}}).hosts(Rule{Group: "web"})[0] != noHost {
		t.Fatal("an evaluator that cannot resolve groups sees no hosts rather than all")
	}
	// status rules: only what is down on the group's hosts
	down := func(kind, group string) string {
		s, _, err := ev.Eval(context.Background(), Rule{Kind: KindStatus, Status: kind, Group: group, Tenant: "acme"}, now)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range s {
			out = append(out, x.Labels["host"]+x.Labels["service"]+x.Labels["container"]+x.Labels["instance"])
		}
		return strings.Join(out, ",")
	}
	if got := down("host_down", ""); got != "db1,web1" {
		t.Fatalf("without a group every host that is down: %q", got)
	}
	if got := down("host_down", "web"); got != "web1" {
		t.Fatalf("with a group, only the group's: %q", got)
	}
	if got := down("host_down", "empty"); got != "" {
		t.Fatalf("%q", got)
	}
	if got := down("service_down", "web"); got != "web1nginx" {
		t.Fatalf("%q", got)
	}
	if got := down("container_down", "web"); got != "web1app" {
		t.Fatalf("%q", got)
	}
	if got := down("instance_down", "web"); got != "ks" {
		t.Fatalf("an instance belongs to a group through the host that checks it: %q", got)
	}
	// the backtest uses the same hosts
	ev.Backtest(context.Background(), Rule{Kind: KindMetric, Metric: "cpu", Reduce: "avg", WindowSec: 60, Op: ">", Threshold: 1, Group: "web", Tenant: "acme"}, now.Add(-time.Hour), now)
	if strings.Join(q.last.Hosts, ",") != "web1,web2" {
		t.Fatalf("%v", q.last.Hosts)
	}
}

func TestTheGroupOfARuleIsChecked(t *testing.T) {
	r := Rule{Name: "r", Kind: KindStatus, Status: "host_down", Group: "  Web   servers "}
	if err := r.Normalize(); err != nil || r.Group != "Web servers" {
		t.Fatalf("%v %q", err, r.Group)
	}
	bad := Rule{Name: "r", Kind: KindStatus, Status: "host_down", Group: "a,b"}
	if err := bad.Normalize(); err == nil || !strings.Contains(err.Error(), "host group") {
		t.Fatalf("%v", err)
	}
	none := Rule{Name: "r", Kind: KindStatus, Status: "host_down"}
	if err := none.Normalize(); err != nil || none.Group != "" {
		t.Fatal("no group is fine")
	}
}

type fakeHealth struct{ r health.Report }

func (f fakeHealth) Health(_ context.Context, tenant string) (health.Report, error) {
	if tenant != "owner" { // only the owner of the installation sees it
		return health.Report{}, nil
	}
	return f.r, nil
}

func TestRulesAboutLumenItself(t *testing.T) {
	const gb = 1 << 30
	rep := health.Report{
		Disks: []health.Disk{{Name: "data", Path: "/data", Total: 100 * gb, Free: 8 * gb}}, // 92%
		ES:    &health.ES{Status: "red", Nodes: []health.ESNode{{Name: "n1", Total: 100 * gb, Avail: 12 * gb}}},
		CH:    &health.CH{Disks: []health.Disk{{Name: "default", Total: 100 * gb, Free: 30 * gb}}},
	}
	ev := &Evaluator{H: fakeHealth{rep}, Q: &captureQ{}, St: fixedStatus{}}
	run := func(status string, thr float64, tenant string) []string {
		s, _, err := ev.Eval(context.Background(), Rule{Kind: KindStatus, Status: status, Threshold: thr, Tenant: tenant}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range s {
			out = append(out, x.Labels["disk"]+x.Labels["component"]+x.Labels["state"]+x.Labels["problem"])
		}
		return out
	}
	if got := strings.Join(run("lumen_disk", 80, "owner"), "|"); got != "Elasticsearch (node n1)|Lumen's data (/data)" {
		t.Fatalf("at 80 percent the data disk (92) and Elasticsearch's view (88) are both too full: %q", got)
	}
	if got := run("lumen_disk", 90, "owner"); len(got) != 1 || got[0] != "Lumen's data (/data)" {
		t.Fatalf("at 90 percent only the data disk: %v", got)
	}
	if got := run("lumen_disk", 95, "owner"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if got := run("lumen_elasticsearch", 2, "owner"); len(got) != 1 || got[0] != "elasticsearchred" {
		t.Fatalf("%v", got)
	}
	rep.ES.Status = "yellow"
	ev.H = fakeHealth{rep}
	if got := run("lumen_elasticsearch", 2, "owner"); len(got) != 0 {
		t.Fatalf("yellow is not red: %v", got)
	}
	if got := run("lumen_elasticsearch", 1, "owner"); len(got) != 1 || got[0] != "elasticsearchyellow" {
		t.Fatalf("but it is not green: %v", got)
	}
	rep.ES = &health.ES{Err: "refused"}
	ev.H = fakeHealth{rep}
	if got := run("lumen_elasticsearch", 2, "owner"); len(got) != 1 || !strings.Contains(got[0], "cannot be reached") {
		t.Fatalf("an Elasticsearch that cannot be reached is a problem at every level: %v", got)
	}
	if got := run("lumen_clickhouse", 90, "owner"); len(got) != 0 {
		t.Fatalf("ClickHouse is fine at 70 percent: %v", got)
	}
	rep.CH = &health.CH{Err: "refused"}
	ev.H = fakeHealth{rep}
	if got := run("lumen_clickhouse", 90, "owner"); len(got) != 1 || !strings.Contains(got[0], "cannot be reached") {
		t.Fatalf("%v", got)
	}
	// an installation without Elasticsearch has nothing to say about it
	rep.ES = nil
	ev.H = fakeHealth{rep}
	if got := run("lumen_elasticsearch", 1, "owner"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	// nobody but the owner is told about the installation
	if got := run("lumen_disk", 10, "someone-else"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	// without a source the rule says why it cannot be checked, instead of looking fine
	if _, _, err := (&Evaluator{}).Eval(context.Background(), Rule{Kind: KindStatus, Status: "lumen_disk", Threshold: 80}, time.Now()); err == nil {
		t.Fatal("an error that is shown on the rule")
	}
}

func TestRulesAboutLumenAreChecked(t *testing.T) {
	r := Rule{Name: "d", Kind: KindStatus, Status: "lumen_disk"}
	if err := r.Normalize(); err != nil || r.Threshold != 80 {
		t.Fatalf("a disk rule defaults to 80 percent: %v %v", err, r.Threshold)
	}
	for status, thr := range map[string]float64{"lumen_disk": 120, "lumen_clickhouse": -5, "lumen_elasticsearch": 3} {
		bad := Rule{Name: "x", Kind: KindStatus, Status: status, Threshold: thr}
		if bad.Normalize() == nil {
			t.Errorf("%s %v must be refused", status, thr)
		}
	}
	e := Rule{Name: "e", Kind: KindStatus, Status: "lumen_elasticsearch"}
	if err := e.Normalize(); err != nil || e.Threshold != 1 {
		t.Fatalf("%v %v", err, e.Threshold)
	}
	have := map[string]bool{}
	for _, tpl := range Templates() {
		have[tpl.Name] = true
	}
	for _, n := range []string{"Lumen: a disk is more than 80% full", "Lumen: a disk is more than 90% full", "Lumen: Elasticsearch is not green", "Lumen: Elasticsearch is red", "Lumen: ClickHouse cannot be reached or is full"} {
		if !have[n] {
			t.Errorf("template %q", n)
		}
	}
}

func TestTheSameDiskIsOneSampleInTheRule(t *testing.T) {
	const gb = 1 << 30
	rep := health.Report{
		Disks: []health.Disk{{Name: "data", Path: "/data", Total: 70 * gb, Free: 11 * gb}},
		ES:    &health.ES{Status: "green", Nodes: []health.ESNode{{Name: "n1", Total: 70 * gb, Avail: 11 * gb}}},
		CH:    &health.CH{Disks: []health.Disk{{Name: "default", Total: 70 * gb, Free: 11 * gb}}},
	}
	got := healthSamples("lumen_disk", 80, rep)
	if len(got) != 1 || !strings.Contains(got[0].Labels["disk"], "Elasticsearch") || !strings.Contains(got[0].Labels["disk"], "ClickHouse") || !strings.Contains(got[0].Labels["disk"], "/data") {
		t.Fatalf("one alert for one disk, with every part that lives on it named: %+v", got)
	}
}
