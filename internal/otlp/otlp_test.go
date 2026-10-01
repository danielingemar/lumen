package otlp

import "testing"

func TestMetricsGetHostLabelFromResource(t *testing.T) {
	body := []byte(`{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"host"}},{"key":"host.name","value":{"stringValue":"web-1"}}]},
	 "scopeMetrics":[{"metrics":[{"name":"cpu","gauge":{"dataPoints":[{"timeUnixNano":"1700000000000000000","asDouble":0.5,"attributes":[{"key":"core","value":{"stringValue":"0"}}]}]}}]}]}]}`)
	pts, err := DecodeMetrics(body, "acme")
	if err != nil || len(pts) != 1 || pts[0].Attrs["host"] != "web-1" || pts[0].Attrs["core"] != "0" {
		t.Fatalf("%v %+v", err, pts)
	}
	body2 := []byte(`{"resourceMetrics":[{"resource":{"attributes":[{"key":"host.name","value":{"stringValue":"web-1"}}]},
	 "scopeMetrics":[{"metrics":[{"name":"x","gauge":{"dataPoints":[{"timeUnixNano":"1","asDouble":1,"attributes":[{"key":"host","value":{"stringValue":"explicit"}}]}]}}]}]}]}`)
	pts, _ = DecodeMetrics(body2, "acme")
	if pts[0].Attrs["host"] != "explicit" {
		t.Fatal("an explicit host label must not be overwritten")
	}
}
