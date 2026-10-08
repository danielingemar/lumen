package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/model"
	"github.com/danielingemar/lumen/internal/store"
)

type fakeStore struct {
	servicesRows []json.RawMessage // what Services answers, when set
	spans        []model.Span
	logs         []model.LogRecord
	tenant       string // tenant seen by the last query
	lastSeries   model.SeriesQuery
	latest       []model.Latest
	latestErr    error
	latestFor    string
	facets       model.Facets
	facetCalls   int
	lastFacet    string
	lastLogQ     model.LogQuery
	lastTraceQ   model.TraceQuery
	archive      bool
	metrics      int // metric points stored
}

func (f *fakeStore) InsertSpans(_ context.Context, r []model.Span) error {
	f.spans = append(f.spans, r...)
	return nil
}
func (f *fakeStore) InsertLogs(_ context.Context, r []model.LogRecord) error {
	f.logs = append(f.logs, r...)
	return nil
}
func (f *fakeStore) InsertMetrics(_ context.Context, r []model.MetricPoint) error {
	f.metrics += len(r)
	return nil
}
func (f *fakeStore) Series(_ context.Context, t string, q model.SeriesQuery) ([]model.Series, error) {
	f.tenant, f.lastSeries = t, q
	if q.Source == "bad" {
		return nil, fmt.Errorf("unknown source %q", q.Source)
	}
	return []model.Series{{Labels: map[string]string{"host": "h1"}, Points: [][2]float64{{1000, 1.5}}}}, nil
}
func (f *fakeStore) Services(_ context.Context, t string, _, _ time.Time) ([]json.RawMessage, error) {
	f.tenant = t
	if f.servicesRows != nil {
		return f.servicesRows, nil
	}
	return []json.RawMessage{json.RawMessage(`{"svc":"checkout"}`)}, nil
}
func (f *fakeStore) MetricNames(_ context.Context, t, _ string, _, _ time.Time) ([]json.RawMessage, error) {
	f.tenant = t
	return nil, nil
}
func (f *fakeStore) MetricLabels(_ context.Context, t, _ string, _, _ time.Time) ([]json.RawMessage, error) {
	f.tenant = t
	return nil, nil
}
func (f *fakeStore) Facets(_ context.Context, t, source, service string, from, to time.Time) (model.Facets, error) {
	f.tenant = t
	f.facetCalls++
	f.lastFacet = source + "|" + service
	return f.facets, nil
}
func (f *fakeStore) Latest(_ context.Context, t string, _ []string, _ time.Time) ([]model.Latest, error) {
	f.tenant = t
	if f.latestFor != "" && t != f.latestFor { // the real store filters by tenant in SQL
		return nil, f.latestErr
	}
	return f.latest, f.latestErr
}
func (f *fakeStore) Ping(context.Context) error { return nil }
func (f *fakeStore) QueryTraces(_ context.Context, t string, q model.TraceQuery) ([]json.RawMessage, error) {
	f.tenant, f.lastTraceQ = t, q
	return []json.RawMessage{json.RawMessage(`{"trace_id":"abc"}`)}, nil
}
func (f *fakeStore) GetTrace(_ context.Context, t, _ string) ([]json.RawMessage, error) {
	f.tenant = t
	return nil, nil
}
func (f *fakeStore) QueryLogs(ctx context.Context, t string, q model.LogQuery) ([]json.RawMessage, error) {
	f.lastLogQ = q
	f.archive = store.InArchive(ctx)
	f.tenant = t
	return nil, nil
}

const tracesJSON = `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"checkout"}}]},
"scopeSpans":[{"spans":[{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","name":"GET /pay",
"kind":2,"startTimeUnixNano":"1700000000000000000","endTimeUnixNano":"1700000000250000000",
"attributes":[{"key":"http.status_code","value":{"intValue":"500"}}],"status":{"code":2,"message":"boom"}}]}]}]}`

func newTestServer(keys map[string]string) (*httptest.Server, *fakeStore) {
	fs := &fakeStore{}
	s := New(fs, edition.NewAuthenticator(keys), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return httptest.NewServer(s.Handler()), fs
}

func TestIngestTraces(t *testing.T) {
	ts, fs := newTestServer(map[string]string{"k1": "acme"})
	defer ts.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/v1/traces", bytes.NewBufferString(tracesJSON))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer k1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("status=%v err=%v", resp, err)
	}
	if len(fs.spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(fs.spans))
	}
	s := fs.spans[0]
	if s.Tenant != "acme" || s.Service != "checkout" || s.DurationNs != 250_000_000 || s.StatusCode != 2 || s.Attrs["http.status_code"] != "500" {
		t.Fatalf("unexpected span: %+v", s)
	}
}

func TestGzipIngest(t *testing.T) {
	ts, fs := newTestServer(nil) // dev mode
	defer ts.Close()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte(tracesJSON))
	gz.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/v1/traces", &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 || len(fs.spans) != 1 || fs.spans[0].Tenant != "default" {
		t.Fatalf("gzip ingest failed: %d %+v", resp.StatusCode, fs.spans)
	}
}

func TestAuthRequired(t *testing.T) {
	ts, _ := newTestServer(map[string]string{"k1": "acme"})
	defer ts.Close()
	resp, _ := http.Post(ts.URL+"/v1/traces", "application/json", bytes.NewBufferString(tracesJSON))
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestTenantIsolationOnQuery(t *testing.T) {
	ts, fs := newTestServer(map[string]string{"kA": "tenantA", "kB": "tenantB"})
	defer ts.Close()
	for key, want := range map[string]string{"kA": "tenantA", "kB": "tenantB"} {
		req, _ := http.NewRequest("GET", ts.URL+"/api/v1/traces?tenant=tenantA", nil) // client-supplied tenant must be ignored
		req.Header.Set("X-API-Key", key)
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode != 200 || fs.tenant != want {
			t.Fatalf("key %s: status %d, store saw tenant %q, want %q", key, resp.StatusCode, fs.tenant, want)
		}
	}
}

func TestRejectsProtobuf(t *testing.T) {
	ts, _ := newTestServer(nil)
	defer ts.Close()
	resp, _ := http.Post(ts.URL+"/v1/traces", "application/x-protobuf", bytes.NewBufferString("x"))
	if resp.StatusCode != 415 {
		t.Fatalf("want 415, got %d", resp.StatusCode)
	}
}

func TestBadJSON(t *testing.T) {
	ts, _ := newTestServer(nil)
	defer ts.Close()
	resp, _ := http.Post(ts.URL+"/v1/logs", "application/json", bytes.NewBufferString("{nope"))
	if resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestUIServed(t *testing.T) {
	ts, _ := newTestServer(map[string]string{"k1": "acme"})
	defer ts.Close()
	resp, _ := http.Get(ts.URL + "/") // shell needs no auth; data endpoints still do
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !bytes.Contains(b, []byte("<title>Lumen</title>")) {
		t.Fatalf("ui not served: %d", resp.StatusCode)
	}
}
