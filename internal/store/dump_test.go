package store

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/model"
)

// TestDumpForEngine writes the exact DDL and queries to a JSON file so they can be executed by a real
// ClickHouse engine (see scripts/test-sql.py). Skipped unless LUMEN_DUMP_SQL is set.
func TestDumpForEngine(t *testing.T) {
	out := os.Getenv("LUMEN_DUMP_SQL")
	if out == "" {
		t.Skip("set LUMEN_DUMP_SQL=/path/out.json to dump")
	}
	now := time.Now()
	type qq struct {
		Name, SQL string
		Params    map[string]string
	}
	var qs []qq
	add := func(n, s string, p map[string]string) { qs = append(qs, qq{n, s, p}) }
	s, p := buildTracesQuery("acme", model.TraceQuery{From: now.Add(-time.Hour), To: now.Add(time.Hour)})
	add("traces default", s, p)
	s, p = buildTracesQuery("acme", model.TraceQuery{Service: "checkout", MinDurationMs: 10, ErrorsOnly: true, From: now.Add(-time.Hour), To: now.Add(time.Hour), Limit: 10})
	add("traces filtered", s, p)
	s, p = buildLogsQuery("acme", model.LogQuery{From: now.Add(-time.Hour), To: now.Add(time.Hour)})
	add("logs default", s, p)
	s, p = buildLogsQuery("acme", model.LogQuery{Service: "checkout", Severity: "ERROR", Contains: "boom", TraceID: "5b8efff798038103d269b633813fc60c", From: now.Add(-time.Hour), To: now.Add(time.Hour)})
	add("logs filtered", s, p)
	add("get trace", getTraceSQL, map[string]string{"tenant": "acme", "trace": "5b8efff798038103d269b633813fc60c"})
	win := func(q model.SeriesQuery) model.SeriesQuery {
		q.From, q.To = now.Add(-time.Hour), now.Add(time.Hour)
		return q
	}
	addSeries := func(n string, q model.SeriesQuery) {
		plan, err := buildSeriesQuery("acme", win(q))
		if err != nil {
			t.Fatal(err)
		}
		add(n, plan.SQL, plan.Params)
	}
	addSeries("series gauge avg", model.SeriesQuery{Source: "metric", Name: "cpu", Agg: "avg", StepSec: 86400})
	addSeries("series gauge sum by host", model.SeriesQuery{Source: "metric", Name: "cpu", Agg: "sum", GroupBy: "host", StepSec: 86400})
	addSeries("series gauge filtered", model.SeriesQuery{Source: "metric", Name: "cpu", Agg: "max", Filters: map[string]string{"host": "h2"}, StepSec: 86400})
	addSeries("series gauge last", model.SeriesQuery{Source: "metric", Name: "cpu", Agg: "last", GroupBy: "service", StepSec: 86400})
	addSeries("series rate", model.SeriesQuery{Source: "metric", Name: "reqs_total", Agg: "rate", StepSec: 60})
	addSeries("series traces p95 by service", model.SeriesQuery{Source: "traces", Metric: "p95", GroupBy: "service", StepSec: 86400})
	addSeries("series traces error_rate", model.SeriesQuery{Source: "traces", Metric: "error_rate", GroupBy: "service", StepSec: 86400})
	addSeries("series traces rps", model.SeriesQuery{Source: "traces", Metric: "rps", StepSec: 86400})
	addSeries("series logs by severity", model.SeriesQuery{Source: "logs", GroupBy: "severity", StepSec: 86400})
	addSeries("series logs filtered", model.SeriesQuery{Source: "logs", Contains: "boom", Service: "checkout", StepSec: 86400})
	sv, sp := buildServicesQuery("acme", now.Add(-time.Hour), now.Add(time.Hour))
	add("services", sv, sp)
	sv, sp = buildMetricNamesQuery("acme", "", now.Add(-time.Hour), now.Add(time.Hour))
	add("metric names", sv, sp)
	sv, sp = buildMetricLabelsQuery("acme", "cpu", now.Add(-time.Hour), now.Add(time.Hour))
	add("metric labels", sv, sp)
	// backup, archive, retention and up/down queries
	day := time.Now().UTC()
	for _, tb := range tables {
		q, pr, _ := buildTenantsQuery(tb.Name, day)
		add("tenants "+tb.Name, q, pr)
		q, pr, _ = buildExportQuery(tb.Name, "acme", day)
		add("export "+tb.Name, q, pr)
		q, pr, _ = buildUnloadQuery(tb.Name, "acme", day)
		add("unload "+tb.Name, q, pr)
	}
	q, pr := buildLoadedQuery("acme")
	add("loaded days", q, pr)
	q, pr = buildLatestQuery("acme", []string{"nextcloud_up", "container_up"}, now.Add(-time.Hour))
	add("latest", q, pr)
	add("ttl check", ttlQuery, map[string]string{"db": "lumen", "t": "otel_logs"})
	b, _ := json.Marshal(map[string]any{"ddl": schema(30), "archive_ddl": archiveSchema(), "ttl_alter": ttlAlter(tables[1], 14), "queries": qs})
	os.WriteFile(out, b, 0o644)
}
