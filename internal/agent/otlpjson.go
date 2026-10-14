// Package agent implements the collectors and the OTLP/JSON sender used by lumen-agent.
package agent

import (
	"encoding/json"
	"strconv"
)

type Point struct {
	Service string
	Name    string
	Type    string // "gauge" or "sum"
	Value   float64
	Attrs   map[string]string
	TimeNs  int64
}

type Log struct {
	Service  string
	TimeNs   int64
	Body     string
	Severity string
	Attrs    map[string]string
}

func attrs(m map[string]string) []map[string]any {
	out := make([]map[string]any, 0, len(m))
	for k, v := range m {
		out = append(out, map[string]any{"key": k, "value": map[string]any{"stringValue": v}})
	}
	return out
}

func resource(service, host string) map[string]any {
	return map[string]any{"attributes": attrs(map[string]string{"service.name": service, "host.name": host})}
}

// MetricsPayload builds an OTLP/JSON metrics export for one service.
func MetricsPayload(service, host string, pts []Point) ([]byte, error) {
	type key struct{ name, typ string }
	idx := map[key]int{}
	var metrics []map[string]any
	for _, p := range pts {
		k := key{p.Name, p.Type}
		i, ok := idx[k]
		if !ok {
			m := map[string]any{"name": p.Name}
			data := map[string]any{"dataPoints": []map[string]any{}}
			if p.Type == "sum" {
				data["aggregationTemporality"] = 2 // cumulative
				data["isMonotonic"] = true
			}
			m[p.Type] = data
			metrics = append(metrics, m)
			i = len(metrics) - 1
			idx[k] = i
		}
		data := metrics[i][p.Type].(map[string]any)
		data["dataPoints"] = append(data["dataPoints"].([]map[string]any), map[string]any{
			"attributes": attrs(p.Attrs), "timeUnixNano": strconv.FormatInt(p.TimeNs, 10), "asDouble": p.Value,
		})
	}
	return json.Marshal(map[string]any{"resourceMetrics": []any{map[string]any{
		"resource": resource(service, host), "scopeMetrics": []any{map[string]any{"metrics": metrics}},
	}}})
}

// LogsPayload builds an OTLP/JSON logs export for one service.
func LogsPayload(service, host string, logs []Log) ([]byte, error) {
	recs := make([]map[string]any, 0, len(logs))
	for _, l := range logs {
		recs = append(recs, map[string]any{
			"timeUnixNano": strconv.FormatInt(l.TimeNs, 10), "severityText": l.Severity,
			"body": map[string]any{"stringValue": l.Body}, "attributes": attrs(l.Attrs),
		})
	}
	return json.Marshal(map[string]any{"resourceLogs": []any{map[string]any{
		"resource": resource(service, host), "scopeLogs": []any{map[string]any{"logRecords": recs}},
	}}})
}
