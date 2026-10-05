// Package model holds the storage-neutral row types. JSON tags match the
// ClickHouse column names so rows can be sent with FORMAT JSONEachRow.
package model

import "time"

// FormatTime renders unix nanoseconds as a ClickHouse DateTime64(9) literal (UTC).
func FormatTime(ns int64) string {
	return time.Unix(0, ns).UTC().Format("2006-01-02 15:04:05.000000000")
}

type Span struct {
	Tenant        string            `json:"tenant"`
	TraceID       string            `json:"trace_id"`
	SpanID        string            `json:"span_id"`
	ParentSpanID  string            `json:"parent_span_id"`
	Name          string            `json:"name"`
	Service       string            `json:"service"`
	Kind          uint8             `json:"kind"`
	StartTime     string            `json:"start_time"`
	DurationNs    uint64            `json:"duration_ns"`
	StatusCode    uint8             `json:"status_code"`
	StatusMessage string            `json:"status_message"`
	Attrs         map[string]string `json:"attrs"`
	ResourceAttrs map[string]string `json:"resource_attrs"`
}

type LogRecord struct {
	Tenant        string            `json:"tenant"`
	Timestamp     string            `json:"ts"`
	TraceID       string            `json:"trace_id"`
	SpanID        string            `json:"span_id"`
	Severity      string            `json:"severity"`
	Service       string            `json:"service"`
	Body          string            `json:"body"`
	Attrs         map[string]string `json:"attrs"`
	ResourceAttrs map[string]string `json:"resource_attrs"`
}

type MetricPoint struct {
	Tenant    string            `json:"tenant"`
	Timestamp string            `json:"ts"`
	Name      string            `json:"name"`
	Type      string            `json:"type"`
	Service   string            `json:"service"`
	Value     float64           `json:"value"`
	Attrs     map[string]string `json:"attrs"`
}

// Query parameter structs shared by the API and the store.
type TraceQuery struct {
	Service       string
	Host          string // resource attribute host.name
	Operation     string // root span name
	MinDurationMs uint64
	ErrorsOnly    bool
	From, To      time.Time
	Limit         int
}

type LogQuery struct {
	Service  string
	Host     string // resource attribute host.name
	Severity string
	Contains string
	TraceID  string
	From, To time.Time
	Limit    int
}

// Series is one time series for a chart: points are [unix milliseconds, value].
type Series struct {
	Labels map[string]string `json:"labels"`
	Points [][2]float64      `json:"points"`
}

// SeriesQuery describes a chart query over metrics, trace-derived statistics or log counts.
type SeriesQuery struct {
	Source   string // "metric", "traces" or "logs"
	Name     string // metric name (source=metric)
	Agg      string // metric: avg|sum|min|max|last|rate
	Metric   string // traces: requests|errors|error_rate|rps|avg|p50|p95|p99
	Service  string
	Host     string // traces and logs: resource attribute host.name
	GroupBy  string // metric: "", "service" or a label key; traces: "", "service", "name"; logs: "", "severity", "service"
	Filters  map[string]string
	Severity string
	Contains string
	From, To time.Time
	StepSec  int
}

// Latest is the most recent value of one metric series (name + labels).
type Latest struct {
	Name    string            `json:"name"`
	Service string            `json:"service"`
	Attrs   map[string]string `json:"attrs"`
	Value   float64           `json:"v"`
	T       int64             `json:"t"` // unix seconds of the newest sample
}

// Facet is one selectable value (a service, host or operation) with how many rows carry it.
type Facet struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// Facets lists what has been received, to fill dropdowns.
type Facets struct {
	Services   []Facet `json:"services"`
	Hosts      []Facet `json:"hosts"`
	Operations []Facet `json:"operations"`
}
