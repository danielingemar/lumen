package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/danielingemar/lumen/internal/alerts"
	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/perm"
	"github.com/danielingemar/lumen/internal/status"
)

// WithAlerts enables alert rules, notifications and the alerts API.
func (s *Server) WithAlerts(e *alerts.Engine) *Server { s.alerts = e; return s }

// Summary gives the engine the up/down picture of a tenant (shared with the status boxes, cached for a few seconds).
func (s *Server) Summary(ctx context.Context, tenant string) (status.Summary, error) {
	if s.reg == nil { // a server without a host registry has nothing to report
		return status.Summary{}, nil
	}
	sn, err := s.snapshot(ctx, tenant)
	return sn.sum, err
}

func (s *Server) alertRoutes(mux *http.ServeMux) {
	if s.alerts == nil {
		return
	}
	mux.Handle("GET /api/v1/alerts", s.need(perm.Alerts, false, s.listAlerts))
	mux.Handle("POST /api/v1/alerts/ack/{fp}", s.need(perm.Alerts, true, s.ackAlert(true)))
	mux.Handle("DELETE /api/v1/alerts/ack/{fp}", s.need(perm.Alerts, true, s.ackAlert(false)))
	mux.Handle("GET /api/v1/alerts/rules", s.need(perm.Alerts, false, s.listRules))
	mux.Handle("POST /api/v1/alerts/rules", s.need(perm.Alerts, true, s.putRule("")))
	mux.Handle("PUT /api/v1/alerts/rules/{id}", s.need(perm.Alerts, true, s.putRule("id")))
	mux.Handle("DELETE /api/v1/alerts/rules/{id}", s.need(perm.Alerts, true, s.deleteRule))
	mux.Handle("POST /api/v1/alerts/backtest", s.need(perm.Alerts, false, s.backtest))
	mux.Handle("GET /api/v1/alerts/templates", s.need(perm.Alerts, false, func(w http.ResponseWriter, r *http.Request, id edition.Identity) {
		writeJSON(w, map[string]any{"data": alerts.Templates()})
	}))
	mux.Handle("GET /api/v1/alerts/silences", s.need(perm.Alerts, false, func(w http.ResponseWriter, r *http.Request, id edition.Identity) {
		writeJSON(w, map[string]any{"data": s.alerts.ListSilences(id.Tenant)})
	}))
	mux.Handle("POST /api/v1/alerts/silences", s.need(perm.Alerts, true, s.createSilence))
	mux.Handle("DELETE /api/v1/alerts/silences/{id}", s.need(perm.Alerts, true, func(w http.ResponseWriter, r *http.Request, id edition.Identity) {
		s.alertErr(w, s.alerts.DeleteSilence(id.Tenant, r.PathValue("id")), map[string]bool{"ok": true})
	}))
	mux.Handle("GET /api/v1/alerts/history", s.need(perm.Alerts, false, func(w http.ResponseWriter, r *http.Request, id edition.Identity) {
		writeJSON(w, map[string]any{"data": s.alerts.History(id.Tenant, 200)})
	}))
	mux.Handle("GET /api/v1/alerts/deliveries", s.need(perm.Notifications, false, func(w http.ResponseWriter, r *http.Request, id edition.Identity) {
		writeJSON(w, map[string]any{"data": s.alerts.Deliveries(id.Tenant)})
	}))
	// channels hold secrets and send messages from the server: their own permission
	mux.Handle("GET /api/v1/notifications/types", s.need(perm.Notifications, false, s.notifierTypes))
	mux.Handle("GET /api/v1/notifications/channels", s.need(perm.Notifications, false, func(w http.ResponseWriter, r *http.Request, id edition.Identity) {
		writeJSON(w, map[string]any{"data": s.alerts.ListChannels(id.Tenant)})
	}))
	mux.Handle("POST /api/v1/notifications/channels", s.need(perm.Notifications, true, s.putChannel("")))
	mux.Handle("PUT /api/v1/notifications/channels/{id}", s.need(perm.Notifications, true, s.putChannel("id")))
	mux.Handle("DELETE /api/v1/notifications/channels/{id}", s.need(perm.Notifications, true, func(w http.ResponseWriter, r *http.Request, id edition.Identity) {
		s.alertErr(w, s.alerts.DeleteChannel(id.Tenant, r.PathValue("id")), map[string]bool{"ok": true})
	}))
	mux.Handle("POST /api/v1/notifications/channels/{id}/test", s.need(perm.Notifications, true, s.testChannel))
}

// alertErr answers with v when err is nil, otherwise with the right status.
func (s *Server) alertErr(w http.ResponseWriter, err error, v any) {
	switch {
	case err == nil:
		writeJSON(w, v)
	case errors.Is(err, alerts.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, alerts.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	default:
		s.log.Error("alerts error", "err", err)
		writeErr(w, http.StatusBadGateway, err.Error())
	}
}

type alertCounts struct {
	Firing  int `json:"firing"`
	Pending int `json:"pending"`
}

func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	f, p := s.alerts.Counts(id.Tenant)
	writeJSON(w, map[string]any{"data": s.alerts.ListAlerts(id.Tenant), "counts": alertCounts{f, p}})
}

func (s *Server) ackAlert(ack bool) handler {
	return func(w http.ResponseWriter, r *http.Request, id edition.Identity) {
		by := id.User
		if by == "" {
			by = "api key"
		}
		s.alertErr(w, s.alerts.Ack(id.Tenant, r.PathValue("fp"), by, ack), map[string]bool{"ok": true})
	}
}

type ruleOut struct {
	alerts.Rule
	Error string `json:"error,omitempty"` // why the rule cannot be evaluated right now
}

func (s *Server) listRules(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	health := s.alerts.RuleHealth(id.Tenant)
	out := []ruleOut{}
	for _, rule := range s.alerts.ListRules(id.Tenant) {
		out = append(out, ruleOut{rule, health[rule.ID]})
	}
	writeJSON(w, map[string]any{"data": out})
}

func (s *Server) putRule(pathID string) handler {
	return func(w http.ResponseWriter, r *http.Request, id edition.Identity) {
		var in alerts.Rule
		if !readJSON(w, r, &in) {
			return
		}
		rid := ""
		if pathID != "" {
			rid = r.PathValue(pathID)
		}
		out, err := s.alerts.PutRule(id.Tenant, rid, in)
		if err == nil {
			s.log.Info("alert rule saved", "by", id.User, "tenant", id.Tenant, "rule", out.Name)
		}
		s.alertErr(w, err, out)
	}
}

func (s *Server) deleteRule(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	s.alertErr(w, s.alerts.DeleteRule(id.Tenant, r.PathValue("id")), map[string]bool{"ok": true})
}

// backtest replays a rule over the last hours of history (the rule does not need to be saved).
func (s *Server) backtest(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in struct {
		Rule  alerts.Rule `json:"rule"`
		Hours int         `json:"hours"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	area := map[string]string{alerts.KindMetric: perm.Metrics, alerts.KindLog: perm.Logs}[in.Rule.Kind]
	if area != "" && !id.Can(area, false) {
		writeErr(w, http.StatusForbidden, "replaying a rule reads "+area+", which your account may not read")
		return
	}
	if in.Hours <= 0 {
		in.Hours = 24
	}
	if in.Hours > 7*24 {
		in.Hours = 7 * 24
	}
	to := time.Now()
	iv, err := s.alerts.Backtest(r.Context(), id.Tenant, in.Rule, to.Add(-time.Duration(in.Hours)*time.Hour), to)
	if iv == nil {
		iv = []alerts.Interval{}
	}
	s.alertErr(w, err, map[string]any{"data": iv, "hours": in.Hours})
}

func (s *Server) createSilence(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in struct {
		Matchers map[string]string `json:"matchers"`
		Minutes  int               `json:"minutes"`
		Reason   string            `json:"reason"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	by := id.User
	if by == "" {
		by = "api key"
	}
	sil, err := s.alerts.CreateSilence(id.Tenant, in.Matchers, time.Duration(in.Minutes)*time.Minute, in.Reason, by)
	s.alertErr(w, err, sil)
}

func (s *Server) notifierTypes(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	type typ struct {
		Type    string         `json:"type"`
		Label   string         `json:"label"`
		Fields  []alerts.Field `json:"fields"`
		Edition string         `json:"edition"`
		Locked  bool           `json:"locked"` // needs a licence that is not in force
	}
	out := []typ{}
	for _, n := range alerts.Default.Types() {
		out = append(out, typ{n.Type(), n.Label(), n.Fields(), alerts.EditionOf(n), !s.alerts.Allowed(n)})
	}
	writeJSON(w, map[string]any{"data": out})
}

func (s *Server) putChannel(pathID string) handler {
	return func(w http.ResponseWriter, r *http.Request, id edition.Identity) {
		var in alerts.ChannelIn
		if !readJSON(w, r, &in) {
			return
		}
		cid := ""
		if pathID != "" {
			cid = r.PathValue(pathID)
		}
		out, err := s.alerts.PutChannel(id.Tenant, cid, in)
		if err == nil {
			s.log.Info("notification channel saved", "by", id.User, "tenant", id.Tenant, "channel", out.Name, "type", out.Type)
		}
		s.alertErr(w, err, out)
	}
}

func (s *Server) testChannel(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	err := s.alerts.TestChannel(r.Context(), id.Tenant, r.PathValue("id"))
	if err != nil && !errors.Is(err, alerts.ErrNotFound) && !errors.Is(err, alerts.ErrInvalid) {
		// the destination refused or was unreachable: report it as a result of the test, not as a server error
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.alertErr(w, err, map[string]any{"ok": true})
}

var _ = strconv.Itoa
