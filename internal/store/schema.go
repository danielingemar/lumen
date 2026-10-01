package store

import (
	"fmt"
	"strings"
)

// tables lists the telemetry tables and the column their expiry (and daily backup) is based on.
var tables = []struct{ Name, TimeCol string }{{"otel_spans", "start_time"}, {"otel_logs", "ts"}, {"otel_metrics", "ts"}}

const ttlQuery = "SELECT create_table_query FROM system.tables WHERE database = {db:String} AND name = {t:String} FORMAT JSONEachRow"

// ttlIs reports whether a table definition (system.tables.create_table_query) already expires after `days`.
// ClickHouse prints INTERVAL 30 DAY as toIntervalDay(30).
func ttlIs(create string, days int) bool {
	return strings.Contains(create, fmt.Sprintf("toIntervalDay(%d)", days)) || strings.Contains(create, fmt.Sprintf("INTERVAL %d DAY", days))
}

func ttlAlter(t struct{ Name, TimeCol string }, days int) string {
	return fmt.Sprintf("ALTER TABLE %s MODIFY TTL toDateTime(%s) + INTERVAL %d DAY", t.Name, t.TimeCol, days)
}

// archiveSchema creates <table>_archive tables: the same columns, no expiry. Restored backups are loaded here.
func archiveSchema() []string {
	var out []string
	for _, ddl := range schema(1) {
		if i := strings.Index(ddl, "\nTTL "); i >= 0 {
			ddl = ddl[:i]
		}
		for _, t := range tables {
			ddl = strings.Replace(ddl, "IF NOT EXISTS "+t.Name+" (", "IF NOT EXISTS "+t.Name+"_archive (", 1)
		}
		out = append(out, ddl)
	}
	return out
}

func schema(retentionDays int) []string {
	ttl := func(col string) string {
		return fmt.Sprintf("TTL toDateTime(%s) + INTERVAL %d DAY", col, retentionDays)
	}
	return []string{
		`CREATE TABLE IF NOT EXISTS otel_spans (
  tenant LowCardinality(String),
  trace_id String,
  span_id String,
  parent_span_id String,
  name LowCardinality(String),
  service LowCardinality(String),
  kind UInt8,
  start_time DateTime64(9) CODEC(Delta, ZSTD(1)),
  duration_ns UInt64 CODEC(T64, ZSTD(1)),
  status_code UInt8,
  status_message String CODEC(ZSTD(1)),
  attrs Map(String, String) CODEC(ZSTD(1)),
  resource_attrs Map(String, String) CODEC(ZSTD(1)),
  INDEX idx_trace_id trace_id TYPE bloom_filter(0.01) GRANULARITY 1
) ENGINE = MergeTree
PARTITION BY toDate(start_time)
ORDER BY (tenant, service, toStartOfHour(start_time), trace_id)
` + ttl("start_time"),

		`CREATE TABLE IF NOT EXISTS otel_logs (
  tenant LowCardinality(String),
  ts DateTime64(9) CODEC(Delta, ZSTD(1)),
  trace_id String,
  span_id String,
  severity LowCardinality(String),
  service LowCardinality(String),
  body String CODEC(ZSTD(1)),
  attrs Map(String, String) CODEC(ZSTD(1)),
  resource_attrs Map(String, String) CODEC(ZSTD(1)),
  INDEX idx_trace_id trace_id TYPE bloom_filter(0.01) GRANULARITY 1
) ENGINE = MergeTree
PARTITION BY toDate(ts)
ORDER BY (tenant, service, severity, toStartOfHour(ts), ts)
` + ttl("ts"),

		`CREATE TABLE IF NOT EXISTS otel_metrics (
  tenant LowCardinality(String),
  ts DateTime64(9) CODEC(Delta, ZSTD(1)),
  name LowCardinality(String),
  type LowCardinality(String),
  service LowCardinality(String),
  value Float64 CODEC(Gorilla, ZSTD(1)),
  attrs Map(String, String) CODEC(ZSTD(1))
) ENGINE = MergeTree
PARTITION BY toDate(ts)
ORDER BY (tenant, service, name, toStartOfHour(ts), ts)
` + ttl("ts"),
	}
}
