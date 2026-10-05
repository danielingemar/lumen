package store

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/danielingemar/lumen/internal/config"
	"github.com/danielingemar/lumen/internal/model"
)

// ClickHouse talks to ClickHouse over its HTTP interface. No driver dependency:
// inserts use FORMAT JSONEachRow and queries use server-side typed parameters
// ({name:Type}), so user input is never concatenated into SQL.
type ClickHouse struct {
	cfg  config.Config
	http *http.Client
	long *http.Client // no overall timeout: backup exports and imports can run for minutes (the context still limits them)
}

func NewClickHouse(cfg config.Config) *ClickHouse {
	return &ClickHouse{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}, long: &http.Client{}}
}

func (c *ClickHouse) do(ctx context.Context, query string, params map[string]string, body io.Reader) ([]byte, error) {
	resp, err := c.send(ctx, c.http, query, params, nil, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("clickhouse: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// send issues a request and returns the raw response; the caller closes the body and checks the status.
func (c *ClickHouse) send(ctx context.Context, cl *http.Client, query string, params, settings map[string]string, body io.Reader) (*http.Response, error) {
	if isArchive(ctx) {
		query = archiveSQL(query)
	}
	u, err := url.Parse(c.cfg.ClickHouseURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("query", query)
	q.Set("database", c.cfg.ClickHouseDB)
	q.Set("output_format_json_quote_64bit_integers", "0")
	for k, v := range params {
		q.Set("param_"+k, v)
	}
	for k, v := range settings {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.cfg.ClickHouseUser, c.cfg.ClickHousePass)
	return cl.Do(req)
}

// Migrate creates the database and tables if they do not exist.
func (c *ClickHouse) Migrate(ctx context.Context) error {
	// database creation must not select the (possibly missing) database
	boot := *c
	boot.cfg.ClickHouseDB = "default"
	if _, err := boot.do(ctx, "CREATE DATABASE IF NOT EXISTS "+c.cfg.ClickHouseDB, nil, nil); err != nil {
		return err
	}
	for _, ddl := range schema(c.cfg.RetentionDays) {
		if _, err := c.do(ctx, ddl, nil, nil); err != nil {
			return err
		}
	}
	for _, ddl := range archiveSchema() { // restored backups live here, without expiry
		if _, err := c.do(ctx, ddl, nil, nil); err != nil {
			return err
		}
	}
	// a changed LUMEN_RETENTION_DAYS must also apply to tables that already exist
	for _, t := range tables {
		cur, err := c.do(ctx, ttlQuery, map[string]string{"db": c.cfg.ClickHouseDB, "t": t.Name}, nil)
		if err != nil {
			return err
		}
		if !ttlIs(string(cur), c.cfg.RetentionDays) {
			if _, err := c.do(ctx, ttlAlter(t, c.cfg.RetentionDays), nil, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *ClickHouse) Ping(ctx context.Context) error {
	_, err := c.do(ctx, "SELECT 1", nil, nil)
	return err
}

func insert[T any](ctx context.Context, c *ClickHouse, table string, rows []T) error {
	if len(rows) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := range rows {
		if err := enc.Encode(rows[i]); err != nil {
			return err
		}
	}
	_, err := c.do(ctx, "INSERT INTO "+table+" FORMAT JSONEachRow", nil, &buf)
	return err
}

func (c *ClickHouse) InsertSpans(ctx context.Context, r []model.Span) error {
	return insert(ctx, c, "otel_spans", r)
}
func (c *ClickHouse) InsertLogs(ctx context.Context, r []model.LogRecord) error {
	return insert(ctx, c, "otel_logs", r)
}
func (c *ClickHouse) InsertMetrics(ctx context.Context, r []model.MetricPoint) error {
	return insert(ctx, c, "otel_metrics", r)
}

func lines(data []byte) []json.RawMessage {
	out := []json.RawMessage{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		if b := bytes.TrimSpace(sc.Bytes()); len(b) > 0 {
			out = append(out, json.RawMessage(append([]byte(nil), b...)))
		}
	}
	return out
}

func clampLimit(n, def, max int) int {
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

func window(from, to time.Time) (string, string) {
	if to.IsZero() {
		to = time.Now()
	}
	if from.IsZero() {
		from = to.Add(-1 * time.Hour)
	}
	return model.FormatTime(from.UnixNano()), model.FormatTime(to.UnixNano())
}

func buildTracesQuery(tenant string, q model.TraceQuery) (string, map[string]string) {
	from, to := window(q.From, q.To)
	p := map[string]string{
		"tenant": tenant, "from": from, "to": to,
		"minDur": strconv.FormatUint(q.MinDurationMs*1_000_000, 10),
		"limit":  strconv.Itoa(clampLimit(q.Limit, 50, 500)),
	}
	where := "tenant = {tenant:String} AND start_time >= {from:DateTime64(9)} AND start_time <= {to:DateTime64(9)}"
	if q.Service != "" {
		where += " AND service = {service:String}"
		p["service"] = q.Service
	}
	if q.Host != "" {
		where += " AND resource_attrs['host.name'] = {host:String}"
		p["host"] = q.Host
	}
	having := "t_dur >= {minDur:UInt64}"
	if q.Operation != "" {
		having += " AND root_name = {op:String}"
		p["op"] = q.Operation
	}
	if q.ErrorsOnly {
		having += " AND has_error = 1"
	}
	sql := `SELECT trace_id, root_name, root_service, t_start AS start_time, t_dur AS duration_ns, span_count, has_error
FROM (
  SELECT trace_id,
    argMinIf(name, start_time, parent_span_id = '') AS root_name,
    argMinIf(service, start_time, parent_span_id = '') AS root_service,
    min(start_time) AS t_start,
    max(toUnixTimestamp64Nano(start_time) + duration_ns) - min(toUnixTimestamp64Nano(start_time)) AS t_dur,
    count() AS span_count,
    max(status_code = 2) AS has_error
  FROM otel_spans WHERE ` + where + `
  GROUP BY trace_id HAVING ` + having + `
)
ORDER BY t_start DESC LIMIT {limit:UInt32} FORMAT JSONEachRow`
	return sql, p
}

func (c *ClickHouse) QueryTraces(ctx context.Context, tenant string, q model.TraceQuery) ([]json.RawMessage, error) {
	sql, p := buildTracesQuery(tenant, q)
	data, err := c.do(ctx, sql, p, nil)
	if err != nil {
		return nil, err
	}
	return lines(data), nil
}

const getTraceSQL = `SELECT trace_id, span_id, parent_span_id, name, service, kind, start_time, duration_ns,
  status_code, status_message, attrs, resource_attrs
FROM otel_spans WHERE tenant = {tenant:String} AND trace_id = {trace:String}
ORDER BY start_time LIMIT 10000 FORMAT JSONEachRow`

func (c *ClickHouse) GetTrace(ctx context.Context, tenant, traceID string) ([]json.RawMessage, error) {
	data, err := c.do(ctx, getTraceSQL, map[string]string{"tenant": tenant, "trace": traceID}, nil)
	if err != nil {
		return nil, err
	}
	return lines(data), nil
}

func buildLogsQuery(tenant string, q model.LogQuery) (string, map[string]string) {
	from, to := window(q.From, q.To)
	p := map[string]string{
		"tenant": tenant, "from": from, "to": to,
		"limit": strconv.Itoa(clampLimit(q.Limit, 100, 1000)),
	}
	where := "tenant = {tenant:String} AND ts >= {from:DateTime64(9)} AND ts <= {to:DateTime64(9)}"
	for _, f := range []struct{ val, name, cond string }{
		{q.Service, "service", "service = {service:String}"},
		{q.Host, "host", "resource_attrs['host.name'] = {host:String}"},
		{q.Severity, "severity", "severity = {severity:String}"},
		{q.TraceID, "trace", "trace_id = {trace:String}"},
		{q.Contains, "contains", "positionCaseInsensitive(body, {contains:String}) > 0"},
	} {
		if f.val != "" {
			where += " AND " + f.cond
			p[f.name] = f.val
		}
	}
	sql := `SELECT ts, severity, service, body, trace_id, span_id, attrs
FROM otel_logs WHERE ` + where + ` ORDER BY ts DESC LIMIT {limit:UInt32} FORMAT JSONEachRow`
	return sql, p
}

func (c *ClickHouse) QueryLogs(ctx context.Context, tenant string, q model.LogQuery) ([]json.RawMessage, error) {
	sql, p := buildLogsQuery(tenant, q)
	data, err := c.do(ctx, sql, p, nil)
	if err != nil {
		return nil, err
	}
	return lines(data), nil
}
