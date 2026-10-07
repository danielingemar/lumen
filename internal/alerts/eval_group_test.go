package alerts

import (
	"context"
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
