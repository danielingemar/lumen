// Package server exposes the OTLP/HTTP ingest endpoints and the query API.
package server

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"github.com/danielingemar/lumen/internal/health"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielingemar/lumen/internal/alerts"
	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/backup"
	"github.com/danielingemar/lumen/internal/branding"
	"github.com/danielingemar/lumen/internal/dashboards"
	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/install"
	"github.com/danielingemar/lumen/internal/license"
	"github.com/danielingemar/lumen/internal/model"
	"github.com/danielingemar/lumen/internal/otlp"
	"github.com/danielingemar/lumen/internal/perm"
	"github.com/danielingemar/lumen/internal/registry"
	"github.com/danielingemar/lumen/internal/store"
	"github.com/danielingemar/lumen/internal/ui"
)

const maxBody = 16 << 20 // 16 MiB after decompression

type Server struct {
	dash               *dashboards.Service
	reg                *registry.Service
	bk                 *backup.Manager
	brand              *branding.Service
	alerts             *alerts.Engine
	lic                *license.Manager
	owner              string
	ten                *Tenancy
	health             *health.Monitor
	bkInfo             BackupInfo
	snaps              snapCache
	facetCache         facetCache
	a                  *auth.Auth
	publicURL, distDir string
	installOn          bool
	store              store.Store
	auth               edition.Authenticator
	log                *slog.Logger
}

func New(s store.Store, a edition.Authenticator, l *slog.Logger) *Server {
	return &Server{store: s, auth: a, log: l}
}

// WithInstall enables /install/agent.{sh,ps1} and /download/{file} for one-command agent enrolment.
func (s *Server) WithInstall(publicURL, distDir string) *Server {
	s.publicURL, s.distDir, s.installOn = publicURL, distDir, true
	return s
}

type ctxKey struct{}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", ui.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.Ping(r.Context()); err != nil {
			http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	if s.installOn {
		mux.HandleFunc("GET /install/agent.sh", install.Script("agent.sh", "text/x-shellscript; charset=utf-8", s.publicURL))
		mux.HandleFunc("GET /install/agent-docker.sh", install.Script("agent-docker.sh", "text/x-shellscript; charset=utf-8", s.publicURL))
		mux.HandleFunc("GET /install/agent.ps1", install.Script("agent.ps1", "text/plain; charset=utf-8", s.publicURL))
		mux.HandleFunc("GET /download/{name}", install.Download(s.distDir))
	}
	s.authRoutes(mux)
	s.dashRoutes(mux)
	s.userRoutes(mux)
	s.regRoutes(mux)
	s.backupRoutes(mux)
	s.brandingRoutes(mux)
	s.alertRoutes(mux)
	s.licenseRoutes(mux)
	s.tenancyRoutes(mux)
	s.healthRoutes(mux)
	mux.Handle("POST /v1/traces", s.authed("ingest", s.ingestTraces))
	mux.Handle("POST /v1/logs", s.authed("ingest", s.ingestLogs))
	mux.Handle("POST /v1/metrics", s.authed("ingest", s.ingestMetrics))
	mux.Handle("GET /api/v1/traces", s.need(perm.Traces, false, s.listTraces))
	mux.Handle("GET /api/v1/traces/{id}", s.need(perm.Traces, false, s.getTrace))
	mux.Handle("GET /api/v1/logs", s.need(perm.Logs, false, s.listLogs))
	mux.Handle("GET /api/v1/facets", s.needAny([]string{perm.Traces, perm.Logs}, s.facets))
	mux.Handle("GET /api/v1/services", s.needAny([]string{perm.Traces, perm.Logs, perm.Metrics}, s.services))
	mux.Handle("GET /api/v1/metrics/names", s.need(perm.Metrics, false, s.metricNames))
	mux.Handle("GET /api/v1/metrics/labels", s.need(perm.Metrics, false, s.metricLabels))
	mux.Handle("GET /api/v1/series", s.needAny([]string{perm.Traces, perm.Logs, perm.Metrics}, s.series))
	return mux
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, int, error) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		return nil, http.StatusUnsupportedMediaType,
			errors.New("only OTLP/HTTP JSON is supported; set the exporter encoding to json")
	}
	var src io.Reader = http.MaxBytesReader(w, r.Body, maxBody)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(src)
		if err != nil {
			return nil, http.StatusBadRequest, errors.New("invalid gzip body")
		}
		defer gz.Close()
		src = io.LimitReader(gz, maxBody)
	}
	b, err := io.ReadAll(src)
	if err != nil {
		return nil, http.StatusRequestEntityTooLarge, errors.New("request body too large")
	}
	return b, 0, nil
}

func (s *Server) ingest(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, body []byte) error) {
	body, code, err := readBody(w, r)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	if err := do(r.Context(), body); err != nil {
		var ae *admitError
		if errors.As(err, &ae) { // refused by a limit or because the tenant is suspended: not an error of ours
			retryAfter(w, ae.retry)
			writeErr(w, ae.code, ae.msg)
			return
		}
		if errors.Is(err, errDropped) { // suspended with "drop": answered as accepted, thrown away
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("{}"))
			return
		}
		var de *decodeError
		if errors.As(err, &de) {
			writeErr(w, http.StatusBadRequest, "invalid OTLP JSON: "+de.Error())
			return
		}
		s.log.Error("ingest failed", "err", err)
		writeErr(w, http.StatusServiceUnavailable, "storage error, retry later")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("{}")) // empty ExportServiceResponse
}

type decodeError struct{ error }

// metricHosts are the distinct hosts a batch of metric points came from (at most 500: more is not a host list).
func metricHosts(rows []model.MetricPoint) (hosts, checkers []string) {
	seenH, seenC := map[string]bool{}, map[string]bool{}
	for _, r := range rows { // an agent says what it is in its info line
		if r.Name == "lumen_agent_info" && r.Attrs["role"] == "checker" && r.Attrs["host"] != "" && !seenC[r.Attrs["host"]] {
			seenC[r.Attrs["host"]] = true
			checkers = append(checkers, r.Attrs["host"])
		}
	}
	for _, r := range rows {
		if h := r.Attrs["host"]; h != "" && !seenH[h] && !seenC[h] && len(hosts) < 500 {
			seenH[h] = true
			hosts = append(hosts, h)
		}
	}
	return hosts, checkers
}

func (s *Server) ingestTraces(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	s.ingest(w, r, func(ctx context.Context, b []byte) error {
		rows, err := otlp.DecodeTraces(b, id.Tenant)
		if err != nil {
			return &decodeError{err}
		}
		if err := s.admit(id, len(rows), len(b), nil); err != nil {
			return err
		}
		if err := s.store.InsertSpans(ctx, rows); err != nil {
			return err
		}
		s.metered(id, "traces", len(rows), len(b), nil)
		return nil
	})
}

func (s *Server) ingestLogs(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	s.ingest(w, r, func(ctx context.Context, b []byte) error {
		rows, err := otlp.DecodeLogs(b, id.Tenant)
		if err != nil {
			return &decodeError{err}
		}
		if err := s.admit(id, len(rows), len(b), nil); err != nil {
			return err
		}
		if err := s.store.InsertLogs(ctx, rows); err != nil {
			return err
		}
		s.metered(id, "logs", len(rows), len(b), nil)
		return nil
	})
}

func (s *Server) ingestMetrics(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	s.ingest(w, r, func(ctx context.Context, b []byte) error {
		rows, err := otlp.DecodeMetrics(b, id.Tenant)
		if err != nil {
			return &decodeError{err}
		}
		hosts, checkers := metricHosts(rows)
		if s.ten != nil { // an agent that only checks instances is not a host: it is not counted against the host limit either
			for _, c := range checkers {
				s.ten.Meter.MarkChecker(id.Tenant, c)
			}
			kept := hosts[:0]
			for _, h := range hosts {
				if !s.ten.Meter.IsChecker(id.Tenant, h) {
					kept = append(kept, h)
				}
			}
			hosts = kept
		}
		if err := s.admit(id, len(rows), len(b), hosts); err != nil {
			return err
		}
		if err := s.store.InsertMetrics(ctx, rows); err != nil {
			return err
		}
		s.metered(id, "metrics", len(rows), len(b), hosts)
		return nil
	})
}

// ---- query API ----

func parseTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t
	}
	return time.Time{}
}

func (s *Server) listTraces(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	q := r.URL.Query()
	minMs, _ := strconv.ParseUint(q.Get("min_duration_ms"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	rows, err := s.store.QueryTraces(r.Context(), id.Tenant, model.TraceQuery{
		Service: q.Get("service"), Host: q.Get("host"), Hosts: s.groupHosts(id.Tenant, q.Get("group")), Operation: q.Get("operation"), MinDurationMs: minMs, ErrorsOnly: q.Get("errors") == "true",
		From: parseTime(q.Get("from")), To: parseTime(q.Get("to")), Limit: limit,
	})
	s.respond(w, rows, err)
}

func (s *Server) getTrace(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	rows, err := s.store.GetTrace(r.Context(), id.Tenant, r.PathValue("id"))
	s.respond(w, rows, err)
}

func (s *Server) listLogs(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	rows, err := s.store.QueryLogs(r.Context(), id.Tenant, model.LogQuery{
		Service: q.Get("service"), Host: q.Get("host"), Hosts: s.groupHosts(id.Tenant, q.Get("group")), Severity: q.Get("severity"), Contains: q.Get("q"),
		TraceID: q.Get("trace_id"), From: parseTime(q.Get("from")), To: parseTime(q.Get("to")), Limit: limit,
	})
	s.respond(w, rows, err)
}

func (s *Server) respond(w http.ResponseWriter, rows []json.RawMessage, err error) {
	if err != nil {
		s.log.Error("query failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "query failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": rows})
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// facets lists the services, hosts and root operations that have data, for the dropdowns on the Traces and Logs pages.
// The default window is a week (not the chart range) so that the lists show everything that is coming in; answers are
// cached for a minute because they scan the signal tables.
func (s *Server) facets(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	q := r.URL.Query()
	source := q.Get("source")
	area := map[string]string{"traces": perm.Traces, "logs": perm.Logs}[source]
	if area == "" {
		writeErr(w, http.StatusBadRequest, "source must be traces or logs")
		return
	}
	if !id.Can(area, false) {
		writeErr(w, http.StatusForbidden, "your account does not have access to "+area)
		return
	}
	from, to := parseTime(q.Get("from")), parseTime(q.Get("to"))
	if to.IsZero() {
		to = time.Now()
	}
	if from.IsZero() {
		from = to.Add(-7 * 24 * time.Hour)
	}
	key := strings.Join([]string{id.Tenant, source, q.Get("service"), from.Truncate(time.Minute).String(), to.Truncate(time.Minute).String(), strconv.FormatBool(store.InArchive(r.Context()))}, "|")
	if f, ok := s.facetCache.get(key); ok {
		writeJSON(w, f)
		return
	}
	f, err := s.store.Facets(r.Context(), id.Tenant, source, q.Get("service"), from, to)
	if err != nil {
		s.log.Error("facets query failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "query failed")
		return
	}
	s.facetCache.put(key, f)
	writeJSON(w, f)
}

type facetCache struct {
	mu sync.Mutex
	m  map[string]facetEntry
}
type facetEntry struct {
	at time.Time
	f  model.Facets
}

func (c *facetCache) get(k string) (model.Facets, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || time.Since(e.at) > time.Minute {
		return model.Facets{}, false
	}
	return e.f, true
}

func (c *facetCache) put(k string, f model.Facets) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) > 500 {
		c.m = map[string]facetEntry{}
	}
	c.m[k] = facetEntry{time.Now(), f}
}
