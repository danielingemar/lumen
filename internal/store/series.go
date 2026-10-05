package store

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/danielingemar/lumen/internal/model"
)

var keyRe = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,64}$`)

// seriesPlan is a parameterised SQL statement plus how to interpret its rows.
type seriesPlan struct {
	SQL      string
	Params   map[string]string
	Rate     bool   // rows are (b, g, sk, iv) per underlying series and need differentiating in Go
	LabelKey string // label name for the group column
}

const maxGroups = 12

func windowOrDefault(q model.SeriesQuery) (string, string) { return window(q.From, q.To) }

func buildSeriesQuery(tenant string, q model.SeriesQuery) (seriesPlan, error) {
	from, to := windowOrDefault(q)
	step := q.StepSec
	if step < 1 {
		step = 60
	}
	p := map[string]string{"tenant": tenant, "from": from, "to": to, "step": strconv.Itoa(step)}
	switch q.Source {
	case "metric":
		return metricPlan(q, p)
	case "traces":
		return tracesPlan(q, p)
	case "logs":
		return logsPlan(q, p)
	}
	return seriesPlan{}, fmt.Errorf("unknown source %q", q.Source)
}

func sortedKeys(m map[string]string) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

func metricPlan(q model.SeriesQuery, p map[string]string) (seriesPlan, error) {
	if q.Name == "" {
		return seriesPlan{}, fmt.Errorf("a metric name is required")
	}
	p["name"] = q.Name
	where := "tenant = {tenant:String} AND name = {name:String} AND ts >= {from:DateTime64(9)} AND ts <= {to:DateTime64(9)}"
	if q.Service != "" {
		where += " AND service = {service:String}"
		p["service"] = q.Service
	}
	for i, k := range sortedKeys(q.Filters) {
		if !keyRe.MatchString(k) {
			return seriesPlan{}, fmt.Errorf("invalid filter key %q", k)
		}
		where += fmt.Sprintf(" AND attrs[{fk%d:String}] = {fv%d:String}", i, i)
		p[fmt.Sprintf("fk%d", i)], p[fmt.Sprintf("fv%d", i)] = k, q.Filters[k]
	}
	gexpr, label := "''", ""
	switch {
	case q.GroupBy == "":
	case q.GroupBy == "service":
		gexpr, label = "service", "service"
	case keyRe.MatchString(q.GroupBy):
		gexpr, label = "attrs[{gk:String}]", q.GroupBy
		p["gk"] = q.GroupBy
	default:
		return seriesPlan{}, fmt.Errorf("invalid group_by %q", q.GroupBy)
	}
	inner, outer := "avg", "avg"
	switch q.Agg {
	case "", "avg":
	case "sum":
		outer = "sum"
	case "min":
		inner, outer = "min", "min"
	case "max":
		inner, outer = "max", "max"
	case "last":
		inner, outer = "argMax", "sum"
	case "rate":
		inner = "max"
	default:
		return seriesPlan{}, fmt.Errorf("unknown agg %q", q.Agg)
	}
	innerExpr := inner + "(value)"
	if inner == "argMax" {
		innerExpr = "argMax(value, ts)"
	}
	bucket := "toUnixTimestamp(toStartOfInterval(ts, INTERVAL {step:UInt32} SECOND))"
	sk := "toString(cityHash64(service, toString(attrs)))"
	if q.Agg == "rate" {
		return seriesPlan{Rate: true, LabelKey: label, Params: p, SQL: fmt.Sprintf(
			"SELECT %s AS b, %s AS g, %s AS sk, %s AS iv FROM otel_metrics WHERE %s GROUP BY b, g, sk ORDER BY g, sk, b LIMIT 200000 FORMAT JSONEachRow",
			bucket, gexpr, sk, innerExpr, where)}, nil
	}
	return seriesPlan{LabelKey: label, Params: p, SQL: fmt.Sprintf(
		"SELECT b, g, %s(iv) AS v FROM (SELECT %s AS b, %s AS g, %s AS sk, %s AS iv FROM otel_metrics WHERE %s GROUP BY b, g, sk) GROUP BY b, g ORDER BY g, b LIMIT 200000 FORMAT JSONEachRow",
		outer, bucket, gexpr, sk, innerExpr, where)}, nil
}

func tracesPlan(q model.SeriesQuery, p map[string]string) (seriesPlan, error) {
	exprs := map[string]string{
		"requests": "count()", "errors": "countIf(status_code = 2)",
		"error_rate": "if(count() = 0, 0, countIf(status_code = 2) * 100 / count())",
		"rps":        "count() / {step:UInt32}", "avg": "avg(duration_ns) / 1e6",
		"p50": "quantile(0.5)(duration_ns) / 1e6", "p95": "quantile(0.95)(duration_ns) / 1e6", "p99": "quantile(0.99)(duration_ns) / 1e6",
	}
	m := q.Metric
	if m == "" {
		m = "requests"
	}
	expr, ok := exprs[m]
	if !ok {
		return seriesPlan{}, fmt.Errorf("unknown traces metric %q", m)
	}
	// entry spans: roots and server-side spans, so each service is counted once per request it serves
	where := "tenant = {tenant:String} AND start_time >= {from:DateTime64(9)} AND start_time <= {to:DateTime64(9)} AND (parent_span_id = '' OR kind = 2)"
	if q.Service != "" {
		where += " AND service = {service:String}"
		p["service"] = q.Service
	}
	if q.Host != "" {
		where += " AND resource_attrs['host.name'] = {host:String}"
		p["host"] = q.Host
	}
	gexpr, label := "''", ""
	switch q.GroupBy {
	case "":
	case "service", "name":
		gexpr, label = q.GroupBy, q.GroupBy
	default:
		return seriesPlan{}, fmt.Errorf("traces can be grouped by service or name, not %q", q.GroupBy)
	}
	return seriesPlan{LabelKey: label, Params: p, SQL: fmt.Sprintf(
		"SELECT toUnixTimestamp(toStartOfInterval(start_time, INTERVAL {step:UInt32} SECOND)) AS b, %s AS g, %s AS v FROM otel_spans WHERE %s GROUP BY b, g ORDER BY g, b LIMIT 200000 FORMAT JSONEachRow",
		gexpr, expr, where)}, nil
}

func logsPlan(q model.SeriesQuery, p map[string]string) (seriesPlan, error) {
	where := "tenant = {tenant:String} AND ts >= {from:DateTime64(9)} AND ts <= {to:DateTime64(9)}"
	if q.Service != "" {
		where += " AND service = {service:String}"
		p["service"] = q.Service
	}
	if q.Host != "" {
		where += " AND resource_attrs['host.name'] = {host:String}"
		p["host"] = q.Host
	}
	if q.Severity != "" {
		where += " AND severity = {severity:String}"
		p["severity"] = q.Severity
	}
	if q.Contains != "" {
		where += " AND positionCaseInsensitive(body, {contains:String}) > 0"
		p["contains"] = q.Contains
	}
	gexpr, label := "''", ""
	switch q.GroupBy {
	case "":
	case "severity", "service":
		gexpr, label = q.GroupBy, q.GroupBy
	default:
		return seriesPlan{}, fmt.Errorf("logs can be grouped by severity or service, not %q", q.GroupBy)
	}
	return seriesPlan{LabelKey: label, Params: p, SQL: fmt.Sprintf(
		"SELECT toUnixTimestamp(toStartOfInterval(ts, INTERVAL {step:UInt32} SECOND)) AS b, %s AS g, count() AS v FROM otel_logs WHERE %s GROUP BY b, g ORDER BY g, b LIMIT 200000 FORMAT JSONEachRow",
		gexpr, where)}, nil
}

type seriesRow struct {
	B  float64  `json:"b"`
	G  string   `json:"g"`
	V  *float64 `json:"v"`
	SK string   `json:"sk"`
	IV *float64 `json:"iv"`
}

// assembleSeries turns rows into chart series. Counters (Rate) are differentiated per underlying series,
// treating a decrease as a counter reset, and then summed per group. At most maxGroups groups are kept
// (the largest by total), so a high-cardinality label cannot produce an unreadable chart.
func assembleSeries(rows []seriesRow, plan seriesPlan) []model.Series {
	type key struct{ g string }
	byGroup := map[string]map[float64]float64{}
	add := func(g string, b, v float64) {
		if byGroup[g] == nil {
			byGroup[g] = map[float64]float64{}
		}
		byGroup[g][b] += v
	}
	if plan.Rate {
		type sp struct {
			b, v float64
		}
		per := map[[2]string][]sp{}
		for _, r := range rows {
			if r.IV == nil {
				continue
			}
			k := [2]string{r.G, r.SK}
			per[k] = append(per[k], sp{r.B, *r.IV})
		}
		for k, pts := range per {
			sort.Slice(pts, func(i, j int) bool { return pts[i].b < pts[j].b })
			for i := 1; i < len(pts); i++ {
				dt := pts[i].b - pts[i-1].b
				if dt <= 0 {
					continue
				}
				d := pts[i].v - pts[i-1].v
				if d < 0 { // counter reset: the new value counts from zero
					d = pts[i].v
				}
				add(k[0], pts[i].b, d/dt)
			}
		}
	} else {
		for _, r := range rows {
			if r.V != nil {
				add(r.G, r.B, *r.V)
			}
		}
	}
	type gs struct {
		g     string
		total float64
	}
	var order []gs
	for g, m := range byGroup {
		t := 0.0
		for _, v := range m {
			if v < 0 {
				v = -v
			}
			t += v
		}
		order = append(order, gs{g, t})
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].total != order[j].total {
			return order[i].total > order[j].total
		}
		return order[i].g < order[j].g
	})
	if len(order) > maxGroups {
		order = order[:maxGroups]
	}
	out := make([]model.Series, 0, len(order))
	for _, o := range order {
		bs := make([]float64, 0, len(byGroup[o.g]))
		for b := range byGroup[o.g] {
			bs = append(bs, b)
		}
		sort.Float64s(bs)
		s := model.Series{Labels: map[string]string{}, Points: make([][2]float64, 0, len(bs))}
		if plan.LabelKey != "" {
			s.Labels[plan.LabelKey] = o.g
		}
		for _, b := range bs {
			s.Points = append(s.Points, [2]float64{b * 1000, byGroup[o.g][b]})
		}
		out = append(out, s)
	}
	return out
}

func (c *ClickHouse) Series(ctx context.Context, tenant string, q model.SeriesQuery) ([]model.Series, error) {
	plan, err := buildSeriesQuery(tenant, q)
	if err != nil {
		return nil, err
	}
	data, err := c.do(ctx, plan.SQL, plan.Params, nil)
	if err != nil {
		return nil, err
	}
	var rows []seriesRow
	for _, l := range lines(data) {
		var r seriesRow
		if json.Unmarshal(l, &r) == nil {
			rows = append(rows, r)
		}
	}
	return assembleSeries(rows, plan), nil
}

// ---- discovery queries (dropdowns in the dashboard editor) ----

func buildServicesQuery(tenant string, from, to time.Time) (string, map[string]string) {
	f, t := window(from, to)
	p := map[string]string{"tenant": tenant, "from": f, "to": t}
	sel := func(tbl, col, sig string) string {
		return fmt.Sprintf("SELECT service AS svc, max(%s) AS t, '%s' AS sig FROM %s WHERE tenant = {tenant:String} AND %s >= {from:DateTime64(9)} AND %s <= {to:DateTime64(9)} GROUP BY service", col, sig, tbl, col, col)
	}
	return "SELECT svc, max(t) AS last_seen, groupUniqArray(sig) AS signals FROM (" +
		sel("otel_spans", "start_time", "traces") + " UNION ALL " + sel("otel_logs", "ts", "logs") + " UNION ALL " + sel("otel_metrics", "ts", "metrics") +
		") GROUP BY svc ORDER BY svc LIMIT 500 FORMAT JSONEachRow", p
}

func buildMetricNamesQuery(tenant, service string, from, to time.Time) (string, map[string]string) {
	f, t := window(from, to)
	p := map[string]string{"tenant": tenant, "from": f, "to": t}
	where := "tenant = {tenant:String} AND ts >= {from:DateTime64(9)} AND ts <= {to:DateTime64(9)}"
	if service != "" {
		where += " AND service = {service:String}"
		p["service"] = service
	}
	return "SELECT name, any(type) AS mtype, count() AS points, max(ts) AS last_seen, groupUniqArray(10)(service) AS services FROM otel_metrics WHERE " +
		where + " GROUP BY name ORDER BY name LIMIT 2000 FORMAT JSONEachRow", p
}

func buildMetricLabelsQuery(tenant, name string, from, to time.Time) (string, map[string]string) {
	f, t := window(from, to)
	return "SELECT k AS label, groupUniqArray(25)(attrs[k]) AS vals FROM otel_metrics ARRAY JOIN mapKeys(attrs) AS k " +
			"WHERE tenant = {tenant:String} AND name = {name:String} AND ts >= {from:DateTime64(9)} AND ts <= {to:DateTime64(9)} " +
			"GROUP BY k ORDER BY k LIMIT 50 FORMAT JSONEachRow",
		map[string]string{"tenant": tenant, "name": name, "from": f, "to": t}
}

func (c *ClickHouse) run(ctx context.Context, sql string, p map[string]string) ([]json.RawMessage, error) {
	data, err := c.do(ctx, sql, p, nil)
	if err != nil {
		return nil, err
	}
	return lines(data), nil
}

func (c *ClickHouse) Services(ctx context.Context, tenant string, from, to time.Time) ([]json.RawMessage, error) {
	s, p := buildServicesQuery(tenant, from, to)
	return c.run(ctx, s, p)
}
func (c *ClickHouse) MetricNames(ctx context.Context, tenant, service string, from, to time.Time) ([]json.RawMessage, error) {
	s, p := buildMetricNamesQuery(tenant, service, from, to)
	return c.run(ctx, s, p)
}
func (c *ClickHouse) MetricLabels(ctx context.Context, tenant, name string, from, to time.Time) ([]json.RawMessage, error) {
	s, p := buildMetricLabelsQuery(tenant, name, from, to)
	return c.run(ctx, s, p)
}

var _ = strings.TrimSpace

func buildLatestQuery(tenant string, names []string, from time.Time) (string, map[string]string) {
	p := map[string]string{"tenant": tenant, "from": from.UTC().Format("2006-01-02 15:04:05")}
	var ph []string
	for i, n := range names {
		k := fmt.Sprintf("n%d", i)
		ph = append(ph, "{"+k+":String}")
		p[k] = n
	}
	return "SELECT name, service, attrs, argMax(value, ts) AS v, toUnixTimestamp(max(ts)) AS t FROM otel_metrics WHERE tenant = {tenant:String} AND ts >= {from:DateTime64(9)} AND name IN (" +
		strings.Join(ph, ", ") + ") GROUP BY name, service, attrs ORDER BY name, service LIMIT 50000 FORMAT JSONEachRow", p
}

// Latest returns the newest sample of every series of the named metrics seen since `from`. It drives the
// up/down views: a series whose newest sample is old has stopped reporting.
func (c *ClickHouse) Latest(ctx context.Context, tenant string, names []string, from time.Time) ([]model.Latest, error) {
	q, p := buildLatestQuery(tenant, names, from)
	rows, err := c.run(ctx, q, p)
	if err != nil {
		return nil, err
	}
	out := make([]model.Latest, 0, len(rows))
	for _, r := range rows {
		var l model.Latest
		if json.Unmarshal(r, &l) == nil {
			out = append(out, l)
		}
	}
	return out, nil
}
