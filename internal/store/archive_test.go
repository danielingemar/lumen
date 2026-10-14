package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

var testDay = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)

func TestArchiveSQLRewritesOnlyTableNames(t *testing.T) {
	q := "SELECT name FROM otel_metrics WHERE tenant = {tenant:String} AND name = {n:String} UNION ALL SELECT count() FROM otel_logs JOIN otel_spans USING (trace_id)"
	got := archiveSQL(q)
	for _, want := range []string{"FROM otel_metrics_archive", "FROM otel_logs_archive", "JOIN otel_spans_archive"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if archiveSQL(archiveSQL(q)) != got {
		t.Fatal("rewriting twice must not double-rewrite (otel_logs_archive stays as it is)")
	}
	if archiveSQL("SELECT 'otel_metricsX'") != "SELECT 'otel_metricsX'" {
		t.Fatal("only whole table names are rewritten")
	}
	if !isArchive(WithArchive(context.Background())) || isArchive(context.Background()) {
		t.Fatal("context flag")
	}
}

func TestArchiveSchemaHasNoExpiryAndRetentionCheck(t *testing.T) {
	ddls := archiveSchema()
	if len(ddls) != 3 {
		t.Fatalf("%d archive tables", len(ddls))
	}
	for i, d := range ddls {
		if strings.Contains(d, "TTL") || !strings.Contains(d, tables[i].Name+"_archive (") || strings.Contains(d, "IF NOT EXISTS "+tables[i].Name+" (") {
			t.Errorf("archive DDL %d:\n%s", i, d)
		}
		if !strings.Contains(d, "PARTITION BY toDate(") {
			t.Errorf("archive tables are partitioned by day")
		}
	}
	for _, d := range schema(30) {
		if !strings.Contains(d, "INTERVAL 30 DAY") {
			t.Errorf("live tables expire after the configured retention:\n%s", d)
		}
	}
	if !ttlIs("... TTL toDateTime(ts) + toIntervalDay(30) SETTINGS", 30) || ttlIs("... toIntervalDay(14)", 30) || ttlIs("... toIntervalDay(300)", 30) || !ttlIs("TTL x + INTERVAL 30 DAY", 30) {
		t.Fatal("ttlIs must match exactly the configured number of days")
	}
	if !strings.Contains(ttlAlter(tables[1], 30), "MODIFY TTL toDateTime(ts) + INTERVAL 30 DAY") {
		t.Fatal(ttlAlter(tables[1], 30))
	}
}

func TestBackupQueriesRejectUnknownTables(t *testing.T) {
	if _, _, err := buildExportQuery("users; DROP TABLE x", "acme", testDay); err == nil {
		t.Fatal("only the telemetry tables can be exported")
	}
	if _, _, err := buildUnloadQuery("otel_logs_archive", "acme", testDay); err == nil {
		t.Fatal("table names come from a fixed list")
	}
}
