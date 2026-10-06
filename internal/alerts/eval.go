package alerts

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/danielingemar/lumen/internal/model"
	"github.com/danielingemar/lumen/internal/status"
)

// Querier runs chart queries; it is implemented by the telemetry store. Every call carries the tenant.
type Querier interface {
	Series(ctx context.Context, tenant string, q model.SeriesQuery) ([]model.Series, error)
}

// StatusSource gives the up/down picture of a tenant (hosts, services, containers, Nextcloud instances).
type StatusSource interface {
	Summary(ctx context.Context, tenant string) (status.Summary, error)
}

// Evaluator turns a rule into samples.
type Evaluator struct {
	Q  Querier
	St StatusSource
}

func metricQuery(r Rule, from, to time.Time) model.SeriesQuery {
	q := model.SeriesQuery{Source: "metric", Name: r.Metric, Agg: r.Reduce, GroupBy: r.GroupBy, From: from, To: to, StepSec: 60}
	if len(r.Filters) > 0 {
		q.Filters = map[string]string{}
		for _, f := range r.Filters {
			q.Filters[f.K] = f.V
		}
	}
	return q
}

func logQuery(r Rule, from, to time.Time) model.SeriesQuery {
	return model.SeriesQuery{Source: "logs", Service: r.Service, Host: r.Host, Severity: r.LogSeverity, Contains: r.Contains, GroupBy: r.GroupBy, From: from, To: to, StepSec: 60}
}

// Eval evaluates one rule at time now. noData is true when a metric query returned no series at all.
func (e *Evaluator) Eval(ctx context.Context, rule Rule, now time.Time) (samples []Sample, noData bool, err error) {
	switch rule.Kind {
	case KindStatus:
		sum, err := e.St.Summary(ctx, rule.Tenant)
		if err != nil {
			return nil, false, err
		}
		return statusSamples(rule.Status, sum), false, nil
	case KindMetric:
		series, err := e.Q.Series(ctx, rule.Tenant, metricQuery(rule, now.Add(-time.Duration(rule.WindowSec)*time.Second), now))
		if err != nil {
			return nil, false, err
		}
		for _, s := range series {
			v, ok := Reduce(s.Points, rule.Reduce)
			if !ok {
				continue
			}
			samples = append(samples, Sample{Labels: s.Labels, Value: v, Firing: Compare(v, rule.Op, rule.Threshold)})
		}
		return samples, len(samples) == 0, nil
	case KindLog:
		series, err := e.Q.Series(ctx, rule.Tenant, logQuery(rule, now.Add(-time.Duration(rule.WindowSec)*time.Second), now))
		if err != nil {
			return nil, false, err
		}
		for _, s := range series {
			v, _ := Reduce(s.Points, "sum") // a count of lines
			samples = append(samples, Sample{Labels: s.Labels, Value: v, Firing: Compare(v, rule.Op, rule.Threshold)})
		}
		if len(samples) == 0 { // no lines at all is a count of zero, not "no data"
			samples = []Sample{{Labels: map[string]string{}, Value: 0, Firing: Compare(0, rule.Op, rule.Threshold)}}
		}
		return samples, false, nil
	}
	return nil, false, fmt.Errorf("unknown rule kind %q", rule.Kind)
}

// statusSamples lists what is down right now. Things that are not listed count as "condition false", which resolves them.
func statusSamples(kind string, s status.Summary) []Sample {
	var out []Sample
	add := func(l map[string]string) { out = append(out, Sample{Labels: l, Value: 1, Firing: true}) }
	switch kind {
	case "host_down":
		for _, h := range s.HostList {
			if h.Status == "down" {
				add(map[string]string{"host": h.Name})
			}
		}
	case "instance_down":
		for _, i := range s.Instances {
			if i.Status == "down" {
				add(map[string]string{"instance": i.Name})
			}
		}
	case "service_down", "container_down":
		want := "service"
		if kind == "container_down" {
			want = "container"
		}
		for _, d := range s.Down {
			if d.Kind == want {
				add(map[string]string{"host": d.Host, want: d.Name})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return fmt.Sprint(out[i].Labels) < fmt.Sprint(out[j].Labels) })
	return out
}

// Interval is a period during which a rule would have been firing.
type Interval struct {
	Labels map[string]string `json:"labels"`
	Start  time.Time         `json:"start"`
	End    time.Time         `json:"end"`
	Peak   float64           `json:"peak"`
}

// Backtest replays a metric or log rule over [from, to] and returns when it would have been firing. It uses the same
// reduction and the same "for" rule as the live evaluation, on the stored history of the metric.
func (e *Evaluator) Backtest(ctx context.Context, rule Rule, from, to time.Time) ([]Interval, error) {
	if rule.Kind == KindStatus {
		return nil, fmt.Errorf("status rules cannot be replayed: Lumen does not keep a history of up/down states")
	}
	win := time.Duration(rule.WindowSec) * time.Second
	var q model.SeriesQuery
	if rule.Kind == KindMetric {
		q = metricQuery(rule, from.Add(-win), to)
	} else {
		q = logQuery(rule, from.Add(-win), to)
	}
	series, err := e.Q.Series(ctx, rule.Tenant, q)
	if err != nil {
		return nil, err
	}
	mode := rule.Reduce
	if rule.Kind == KindLog {
		mode = "sum"
	}
	var out []Interval
	for _, s := range series {
		type pt struct {
			t time.Time
			v float64
		}
		var pts []pt
		for _, p := range s.Points {
			pts = append(pts, pt{time.UnixMilli(int64(p[0])), p[1]})
		}
		var runStart time.Time
		var peak float64
		flush := func(end time.Time) {
			if runStart.IsZero() {
				return
			}
			if start := runStart.Add(time.Duration(rule.ForSec) * time.Second); start.Before(end) || start.Equal(end) {
				out = append(out, Interval{Labels: s.Labels, Start: start, End: end, Peak: peak})
			}
			runStart = time.Time{}
		}
		for i, p := range pts {
			if p.t.Before(from) {
				continue
			}
			var win_ [][2]float64
			for j := i; j >= 0 && p.t.Sub(pts[j].t) < win; j-- {
				win_ = append(win_, [2]float64{0, pts[j].v})
			}
			v, ok := Reduce(win_, mode)
			if ok && Compare(v, rule.Op, rule.Threshold) {
				if runStart.IsZero() {
					runStart, peak = p.t, v
				}
				if v > peak {
					peak = v
				}
			} else {
				flush(p.t)
			}
		}
		if len(pts) > 0 {
			flush(pts[len(pts)-1].t)
		}
	}
	return out, nil
}
