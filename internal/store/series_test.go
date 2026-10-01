package store

import (
	"math"
	"testing"

	"github.com/danielingemar/lumen/internal/model"
)

func f(v float64) *float64 { return &v }

func TestAssembleRateHandlesCounterReset(t *testing.T) {
	// one counter, one point per minute: 100,160,220,20(reset),80,140
	var rows []seriesRow
	for i, v := range []float64{100, 160, 220, 20, 80, 140} {
		rows = append(rows, seriesRow{B: float64(1000 + 60*i), G: "", SK: "a", IV: f(v)})
	}
	s := assembleSeries(rows, seriesPlan{Rate: true})
	if len(s) != 1 || len(s[0].Points) != 5 {
		t.Fatalf("want 1 series with 5 points, got %+v", s)
	}
	want := []float64{1, 1, 20.0 / 60, 1, 1}
	for i, p := range s[0].Points {
		if math.Abs(p[1]-want[i]) > 1e-9 || p[0] != float64(1000+60*(i+1))*1000 {
			t.Fatalf("point %d: got %v want %v (timestamps are milliseconds)", i, p, want[i])
		}
	}
}

func TestAssembleRateSumsSeriesPerGroup(t *testing.T) {
	rows := []seriesRow{
		{B: 0, G: "api", SK: "h1", IV: f(0)}, {B: 60, G: "api", SK: "h1", IV: f(60)},
		{B: 0, G: "api", SK: "h2", IV: f(0)}, {B: 60, G: "api", SK: "h2", IV: f(120)},
	}
	s := assembleSeries(rows, seriesPlan{Rate: true, LabelKey: "service"})
	if len(s) != 1 || s[0].Labels["service"] != "api" || s[0].Points[0][1] != 3 {
		t.Fatalf("two hosts at 1/s and 2/s must sum to 3/s: %+v", s)
	}
}

func TestAssembleKeepsLargestGroupsAndSkipsNulls(t *testing.T) {
	var rows []seriesRow
	for i := 0; i < 30; i++ {
		rows = append(rows, seriesRow{B: 60, G: string(rune('a' + i)), V: f(float64(i + 1))})
	}
	rows = append(rows, seriesRow{B: 120, G: "a", V: nil}) // NaN/null from the database
	s := assembleSeries(rows, seriesPlan{LabelKey: "host"})
	if len(s) != maxGroups {
		t.Fatalf("at most %d groups, got %d", maxGroups, len(s))
	}
	if s[0].Labels["host"] != string(rune('a'+29)) {
		t.Fatalf("largest group first, got %v", s[0].Labels)
	}
	for _, x := range s {
		for _, p := range x.Points {
			if math.IsNaN(p[1]) {
				t.Fatal("null values must be dropped, not turned into NaN")
			}
		}
	}
}

func TestSeriesQueryValidation(t *testing.T) {
	bad := []model.SeriesQuery{
		{Source: "nope"}, {Source: "metric"}, // metric needs a name
		{Source: "metric", Name: "x", Agg: "median"}, {Source: "metric", Name: "x", GroupBy: "bad key; DROP"},
		{Source: "metric", Name: "x", Filters: map[string]string{"a b": "c"}},
		{Source: "traces", Metric: "p42"}, {Source: "traces", GroupBy: "host"}, {Source: "logs", GroupBy: "host"},
	}
	for _, q := range bad {
		if _, err := buildSeriesQuery("t", q); err == nil {
			t.Errorf("%+v must be rejected", q)
		}
	}
	// user input only ever travels as parameters, never inside the SQL text
	plan, err := buildSeriesQuery("acme'; DROP TABLE x;--", model.SeriesQuery{Source: "metric", Name: "n'--", Service: "s'x", Filters: map[string]string{"host": "h'1"}, GroupBy: "host"})
	if err != nil {
		t.Fatal(err)
	}
	for _, evil := range []string{"DROP", "n'--", "s'x", "h'1"} {
		if contains(plan.SQL, evil) {
			t.Fatalf("user input %q leaked into the SQL text: %s", evil, plan.SQL)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
