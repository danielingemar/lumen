// Package otlp decodes OTLP/HTTP JSON payloads into storage rows.
package otlp

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/danielingemar/lumen/internal/model"
)

// Int64 accepts both JSON numbers and quoted numbers (OTLP JSON uses strings for 64-bit ints).
type Int64 int64

func (i *Int64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*i = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return err
		}
		v = int64(f)
	}
	*i = Int64(v)
	return nil
}

type AnyValue struct {
	StringValue *string  `json:"stringValue"`
	IntValue    *Int64   `json:"intValue"`
	DoubleValue *float64 `json:"doubleValue"`
	BoolValue   *bool    `json:"boolValue"`
}

func (v AnyValue) String() string {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.IntValue != nil:
		return strconv.FormatInt(int64(*v.IntValue), 10)
	case v.DoubleValue != nil:
		return strconv.FormatFloat(*v.DoubleValue, 'g', -1, 64)
	case v.BoolValue != nil:
		return strconv.FormatBool(*v.BoolValue)
	}
	return ""
}

type KeyValue struct {
	Key   string   `json:"key"`
	Value AnyValue `json:"value"`
}

type Resource struct {
	Attributes []KeyValue `json:"attributes"`
}

func flatten(kv []KeyValue) map[string]string {
	m := make(map[string]string, len(kv))
	for _, a := range kv {
		m[a.Key] = a.Value.String()
	}
	return m
}

func service(res map[string]string) string {
	if s := res["service.name"]; s != "" {
		return s
	}
	return "unknown"
}

// ---- traces ----

type TracesRequest struct {
	ResourceSpans []struct {
		Resource   Resource `json:"resource"`
		ScopeSpans []struct {
			Spans []struct {
				TraceID           string     `json:"traceId"`
				SpanID            string     `json:"spanId"`
				ParentSpanID      string     `json:"parentSpanId"`
				Name              string     `json:"name"`
				Kind              int        `json:"kind"`
				StartTimeUnixNano Int64      `json:"startTimeUnixNano"`
				EndTimeUnixNano   Int64      `json:"endTimeUnixNano"`
				Attributes        []KeyValue `json:"attributes"`
				Status            struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"status"`
			} `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

func DecodeTraces(body []byte, tenant string) ([]model.Span, error) {
	var req TracesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	var out []model.Span
	for _, rs := range req.ResourceSpans {
		res := flatten(rs.Resource.Attributes)
		svc := service(res)
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				dur := int64(s.EndTimeUnixNano) - int64(s.StartTimeUnixNano)
				if dur < 0 {
					dur = 0
				}
				out = append(out, model.Span{
					Tenant: tenant, TraceID: s.TraceID, SpanID: s.SpanID, ParentSpanID: s.ParentSpanID,
					Name: s.Name, Service: svc, Kind: uint8(s.Kind),
					StartTime: model.FormatTime(int64(s.StartTimeUnixNano)), DurationNs: uint64(dur),
					StatusCode: uint8(s.Status.Code), StatusMessage: s.Status.Message,
					Attrs: flatten(s.Attributes), ResourceAttrs: res,
				})
			}
		}
	}
	return out, nil
}

// ---- logs ----

type LogsRequest struct {
	ResourceLogs []struct {
		Resource  Resource `json:"resource"`
		ScopeLogs []struct {
			LogRecords []struct {
				TimeUnixNano         Int64      `json:"timeUnixNano"`
				ObservedTimeUnixNano Int64      `json:"observedTimeUnixNano"`
				SeverityText         string     `json:"severityText"`
				SeverityNumber       int        `json:"severityNumber"`
				Body                 AnyValue   `json:"body"`
				Attributes           []KeyValue `json:"attributes"`
				TraceID              string     `json:"traceId"`
				SpanID               string     `json:"spanId"`
			} `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

func severityFromNumber(n int) string {
	switch {
	case n >= 21:
		return "FATAL"
	case n >= 17:
		return "ERROR"
	case n >= 13:
		return "WARN"
	case n >= 9:
		return "INFO"
	case n >= 5:
		return "DEBUG"
	case n >= 1:
		return "TRACE"
	}
	return "UNSPECIFIED"
}

func DecodeLogs(body []byte, tenant string) ([]model.LogRecord, error) {
	var req LogsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	var out []model.LogRecord
	for _, rl := range req.ResourceLogs {
		res := flatten(rl.Resource.Attributes)
		svc := service(res)
		for _, sl := range rl.ScopeLogs {
			for _, l := range sl.LogRecords {
				ts := int64(l.TimeUnixNano)
				if ts == 0 {
					ts = int64(l.ObservedTimeUnixNano)
				}
				sev := strings.ToUpper(l.SeverityText)
				if sev == "" {
					sev = severityFromNumber(l.SeverityNumber)
				}
				out = append(out, model.LogRecord{
					Tenant: tenant, Timestamp: model.FormatTime(ts), TraceID: l.TraceID, SpanID: l.SpanID,
					Severity: sev, Service: svc, Body: l.Body.String(),
					Attrs: flatten(l.Attributes), ResourceAttrs: res,
				})
			}
		}
	}
	return out, nil
}

// ---- metrics (gauge + sum for now; histograms are a TODO) ----

type numberPoint struct {
	Attributes   []KeyValue `json:"attributes"`
	TimeUnixNano Int64      `json:"timeUnixNano"`
	AsDouble     *float64   `json:"asDouble"`
	AsInt        *Int64     `json:"asInt"`
}

type numberData struct {
	DataPoints []numberPoint `json:"dataPoints"`
}

type MetricsRequest struct {
	ResourceMetrics []struct {
		Resource     Resource `json:"resource"`
		ScopeMetrics []struct {
			Metrics []struct {
				Name  string      `json:"name"`
				Gauge *numberData `json:"gauge"`
				Sum   *numberData `json:"sum"`
			} `json:"metrics"`
		} `json:"scopeMetrics"`
	} `json:"resourceMetrics"`
}

func DecodeMetrics(body []byte, tenant string) ([]model.MetricPoint, error) {
	var req MetricsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	var out []model.MetricPoint
	add := func(name, typ, svc, host string, d *numberData) {
		if d == nil {
			return
		}
		for _, p := range d.DataPoints {
			var v float64
			switch {
			case p.AsDouble != nil:
				v = *p.AsDouble
			case p.AsInt != nil:
				v = float64(*p.AsInt)
			}
			attrs := flatten(p.Attributes)
			if host != "" && attrs["host"] == "" {
				attrs["host"] = host // lets dashboards separate machines that report under the same service name
			}
			out = append(out, model.MetricPoint{
				Tenant: tenant, Timestamp: model.FormatTime(int64(p.TimeUnixNano)),
				Name: name, Type: typ, Service: svc, Value: v, Attrs: attrs,
			})
		}
	}
	for _, rm := range req.ResourceMetrics {
		res := flatten(rm.Resource.Attributes)
		svc, host := service(res), res["host.name"]
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				add(m.Name, "gauge", svc, host, m.Gauge)
				add(m.Name, "sum", svc, host, m.Sum)
			}
		}
	}
	return out, nil
}
