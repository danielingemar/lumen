package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/model"
	"github.com/danielingemar/lumen/internal/perm"
)

const maxRange = 90 * 24 * time.Hour

var niceSteps = []int{5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 21600, 43200, 86400}

// timeRange returns an explicit [from, to] (default: the last hour) and rejects absurd ranges.
func timeRange(r *http.Request) (time.Time, time.Time, error) {
	q := r.URL.Query()
	to, from := parseTime(q.Get("to")), parseTime(q.Get("from"))
	if to.IsZero() {
		to = time.Now()
	}
	if from.IsZero() {
		from = to.Add(-time.Hour)
	}
	if !from.Before(to) {
		return from, to, fmt.Errorf("from must be before to")
	}
	if to.Sub(from) > maxRange {
		return from, to, fmt.Errorf("the time range may be at most 90 days")
	}
	return from, to, nil
}

// pickStep chooses a bucket size giving roughly 120 points, or honours a requested step as long as
// it stays under 1000 points.
func pickStep(rng time.Duration, requested int) int {
	secs := int(rng.Seconds())
	if requested > 0 {
		if min := (secs + 999) / 1000; requested < min {
			requested = min
		}
		return requested
	}
	want := (secs + 119) / 120
	for _, s := range niceSteps {
		if s >= want {
			return s
		}
	}
	return niceSteps[len(niceSteps)-1]
}

func (s *Server) services(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	from, to, err := timeRange(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := s.store.Services(r.Context(), id.Tenant, from, to)
	if err == nil && s.reg != nil {
		rows = s.withoutDismissed(id.Tenant, rows)
	}
	s.respond(w, rows, err)
}

// withoutDismissed leaves out the rows somebody took off the list, unless they have reported since.
func (s *Server) withoutDismissed(tenant string, rows []json.RawMessage) []json.RawMessage {
	gone := s.reg.DismissedServices(tenant)
	if len(gone) == 0 {
		return rows
	}
	out := rows[:0:0]
	for _, r := range rows {
		var x struct {
			Svc      string `json:"svc"`
			LastSeen string `json:"last_seen"`
		}
		if json.Unmarshal(r, &x) == nil {
			if at, ok := gone[x.Svc]; ok {
				if seen, ok := parseSeen(x.LastSeen); !ok || !seen.After(at) {
					continue
				}
			}
		}
		out = append(out, r)
	}
	return out
}

// parseSeen reads the time ClickHouse writes ("2026-10-07 12:34:56.123456789", UTC) or an RFC 3339 one.
func parseSeen(v string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05", time.RFC3339Nano} {
		if t, err := time.ParseInLocation(layout, v, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func (s *Server) dismissService(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := s.reg.DismissService(id.Tenant, in.Name); err != nil {
		s.regErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) metricNames(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	from, to, err := timeRange(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := s.store.MetricNames(r.Context(), id.Tenant, r.URL.Query().Get("service"), from, to)
	s.respond(w, rows, err)
}

func (s *Server) metricLabels(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	from, to, err := timeRange(r)
	name := r.URL.Query().Get("name")
	if err == nil && name == "" {
		err = fmt.Errorf("name is required")
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := s.store.MetricLabels(r.Context(), id.Tenant, name, from, to)
	s.respond(w, rows, err)
}

func (s *Server) series(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	q := r.URL.Query()
	from, to, err := timeRange(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	reqStep, _ := strconv.Atoi(q.Get("step"))
	sq := model.SeriesQuery{
		Source: q.Get("source"), Name: q.Get("name"), Agg: q.Get("agg"), Metric: q.Get("metric"),
		Service: q.Get("service"), Host: q.Get("host"), Hosts: s.groupHosts(id.Tenant, q.Get("group")), GroupBy: q.Get("group_by"), Severity: q.Get("severity"), Contains: q.Get("q"),
		From: from, To: to, StepSec: pickStep(to.Sub(from), reqStep),
	}
	if fl := q["filter"]; len(fl) > 0 {
		if len(fl) > 5 {
			writeErr(w, http.StatusBadRequest, "at most 5 filters")
			return
		}
		sq.Filters = map[string]string{}
		for _, f := range fl {
			k, v, ok := strings.Cut(f, ":")
			if !ok || k == "" {
				writeErr(w, http.StatusBadRequest, "filter must look like key:value")
				return
			}
			sq.Filters[k] = v
		}
	}
	area := map[string]string{"metric": perm.Metrics, "traces": perm.Traces, "logs": perm.Logs}[sq.Source]
	if area != "" && !id.Can(area, false) {
		writeErr(w, http.StatusForbidden, "your account does not have access to "+area)
		return
	}
	series, err := s.store.Series(r.Context(), id.Tenant, sq)
	if err != nil {
		if isUserError(err) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.log.Error("series query failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "query failed")
		return
	}
	if series == nil {
		series = []model.Series{}
	}
	writeJSON(w, map[string]any{"data": series, "step": sq.StepSec})
}

// isUserError reports whether a store error came from validating the request rather than from the database.
func isUserError(err error) bool {
	m := err.Error()
	return !strings.HasPrefix(m, "clickhouse:") && !strings.Contains(m, "connection") && !strings.Contains(m, "deadline")
}
