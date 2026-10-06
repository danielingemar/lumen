package server

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/danielingemar/lumen/internal/audit"
	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/license"
	"github.com/danielingemar/lumen/internal/metering"
	"github.com/danielingemar/lumen/internal/perm"
	"github.com/danielingemar/lumen/internal/tenants"
)

func (s *Server) tenancyRoutes(mux *http.ServeMux) {
	if s.ten == nil {
		return
	}
	// the operator console: the operator permission AND the Operator add-on licence
	mux.Handle("GET /api/v1/operator/tenants", s.op(false, s.listTenants))
	mux.Handle("POST /api/v1/operator/tenants", s.op(true, s.createTenant))
	mux.Handle("GET /api/v1/operator/tenants/{id}", s.op(false, s.getTenant))
	mux.Handle("PUT /api/v1/operator/tenants/{id}", s.op(true, s.updateTenant))
	mux.Handle("POST /api/v1/operator/tenants/{id}/suspend", s.op(true, s.suspendTenant(true)))
	mux.Handle("POST /api/v1/operator/tenants/{id}/resume", s.op(true, s.suspendTenant(false)))
	mux.Handle("POST /api/v1/operator/tenants/{id}/offboard", s.op(true, s.offboardTenant))
	mux.Handle("GET /api/v1/operator/tenants/{id}/export", s.op(true, s.exportTenant))
	mux.Handle("POST /api/v1/operator/tenants/{id}/enter", s.op(true, s.enterTenant))
	mux.Handle("GET /api/v1/operator/usage", s.op(false, s.operatorUsage))
	mux.Handle("GET /api/v1/operator/audit", s.op(false, s.operatorAudit))
	// leaving works from inside a tenant, where the console itself is not available
	mux.Handle("POST /api/v1/operator/leave", s.sessionOnly("", false, s.leaveTenant))
	// what a tenant sees of itself
	mux.Handle("GET /api/v1/usage", s.need(perm.Settings, false, s.ownUsage))
	mux.Handle("GET /api/v1/support-access", s.need(perm.Settings, false, s.getSupport))
	mux.Handle("PUT /api/v1/support-access", s.need(perm.Settings, true, s.putSupport))
	mux.Handle("POST /api/v1/support-access/grant", s.need(perm.Settings, true, s.grantSupport))
	mux.Handle("DELETE /api/v1/support-access/grant", s.need(perm.Settings, true, s.revokeSupport))
	mux.Handle("GET /api/v1/audit", s.need(perm.Settings, false, s.ownAudit))
}

// op is the gate of the operator console. Without the Operator licence the console is closed, and says why.
func (s *Server) op(write bool, h handler) http.Handler {
	return s.need(perm.Operator, write, func(w http.ResponseWriter, r *http.Request, id edition.Identity) {
		if !s.enforced() {
			writeErr(w, http.StatusForbidden, "The tenant console is part of the Operator add-on and needs an Operator licence. Install one under Settings. Nothing is lost without it: tenants keep working and usage is still counted.")
			return
		}
		h(w, r, id)
	})
}

func (s *Server) tenantErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tenants.ErrInvalid):
		writeErr(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), "invalid: "))
	case errors.Is(err, tenants.ErrNotFound):
		writeErr(w, http.StatusNotFound, "no such tenant")
	default:
		s.log.Error("tenant operation failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "that did not work: "+err.Error())
	}
}

type usageOut struct {
	Items      int64           `json:"items"`
	Bytes      int64           `json:"bytes"`
	Traces     metering.Signal `json:"traces"`
	Logs       metering.Signal `json:"logs"`
	Metrics    metering.Signal `json:"metrics"`
	Hosts      int             `json:"hosts"`
	Instances  int             `json:"instances"`
	Users      int             `json:"users"`
	Groups     int             `json:"groups"`
	Keys       int             `json:"keys"`
	Dashboards int             `json:"dashboards"`
}

func (s *Server) usageOf(t tenants.Tenant) (usageOut, []string) {
	u, warn := s.ten.usage(t.ID, t.Quotas)
	d := s.ten.Meter.Today(t.ID)
	if warn == nil {
		warn = []string{}
	}
	return usageOut{Items: d.Items(), Bytes: d.Bytes(), Traces: d.Traces, Logs: d.Logs, Metrics: d.Metrics, Hosts: u.Hosts, Instances: u.Instances, Users: u.Users, Groups: u.Groups, Keys: u.Keys, Dashboards: u.Dashboards}, warn
}

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	type row struct {
		tenants.Tenant
		Usage    usageOut `json:"usage"`
		Warnings []string `json:"warnings"`
	}
	out := []row{}
	for _, t := range s.ten.Tenants.List() {
		if t.Status == tenants.Deleted && t.Offboard == nil {
			continue
		}
		u, w := s.usageOf(t)
		out = append(out, row{t, u, w})
	}
	writeJSON(w, map[string]any{"data": out})
}

type userBrief struct {
	Name    string    `json:"name"`
	Group   string    `json:"group"`
	Created time.Time `json:"created"`
}

func (s *Server) getTenant(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	t, ok := s.ten.Tenants.Get(r.PathValue("id"))
	if !ok {
		s.tenantErr(w, tenants.ErrNotFound)
		return
	}
	u, warn := s.usageOf(t)
	now := time.Now().UTC()
	days := s.ten.Meter.Range(t.ID, now.AddDate(0, 0, -29).Format("2006-01-02"), now.Format("2006-01-02"))
	users := []userBrief{}
	for _, x := range s.a.Store.ListUsersIn(t.ID) {
		users = append(users, userBrief{x.Name, auth.EffectiveGroup(x), x.Created})
	}
	var grant *tenants.Grant
	if g, ok := s.ten.Tenants.ActiveGrant(t.ID); ok {
		grant = &g
	}
	writeJSON(w, map[string]any{"tenant": t, "usage": u, "warnings": warn, "days": days, "users": users, "grant": grant, "audit": s.ten.Audit.List(t.ID, 20)})
}

type quotasIn = tenants.Quotas

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in struct {
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		Contact       string   `json:"contact"`
		Notes         string   `json:"notes"`
		Plan          string   `json:"plan"`
		Quotas        quotasIn `json:"quotas"`
		SupportAccess string   `json:"support_access"`
		AdminUser     string   `json:"admin_user"`
		AdminPassword string   `json:"admin_password"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	admin, err := auth.NormalizeUser(in.AdminUser)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "the first administrator needs a user name: "+err.Error())
		return
	}
	if _, exists := s.a.Store.GetUser(admin); exists {
		writeErr(w, http.StatusBadRequest, "a user called "+admin+" already exists (user names are unique in the whole installation)")
		return
	}
	t, err := s.ten.Tenants.Create(tenants.CreateIn{ID: in.ID, Name: in.Name, Contact: in.Contact, Notes: in.Notes, Plan: in.Plan, Quotas: in.Quotas, SupportAccess: in.SupportAccess})
	if err != nil {
		s.tenantErr(w, err)
		return
	}
	pw, generated := in.AdminPassword, false
	if pw == "" {
		pw, generated = auth.RandomToken(9), true
	}
	if err := s.a.Store.CreateUserIn(admin, t.ID, pw, "admin"); err != nil {
		// the tenant must not be left half made: take the record away again
		c, cancel := ctxShort()
		_ = s.ten.Tenants.DB.Delete(c, "tenants", t.ID)
		cancel()
		writeErr(w, http.StatusBadRequest, "the first administrator could not be created: "+err.Error())
		return
	}
	s.auditOp(id, t.ID, "tenant.create", t.ID, t.Name)
	out := map[string]any{"tenant": t, "admin": map[string]any{"user": admin}}
	if generated {
		out["admin"] = map[string]any{"user": admin, "password": pw, "note": "This password is shown only now. The user can change it after signing in."}
	}
	writeJSON(w, out)
}

func (s *Server) updateTenant(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in struct {
		Name          string   `json:"name"`
		Contact       string   `json:"contact"`
		Notes         string   `json:"notes"`
		Plan          string   `json:"plan"`
		Quotas        quotasIn `json:"quotas"`
		SuspendIngest string   `json:"suspend_ingest"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	old, _ := s.ten.Tenants.Get(r.PathValue("id"))
	t, err := s.ten.Tenants.Update(r.PathValue("id"), tenants.UpdateIn{Name: in.Name, Contact: in.Contact, Notes: in.Notes, Plan: in.Plan, Quotas: in.Quotas, SuspendIngest: in.SuspendIngest})
	if err != nil {
		s.tenantErr(w, err)
		return
	}
	detail := "settings changed"
	if old.Quotas != t.Quotas {
		detail = fmt.Sprintf("limits changed: %+v", t.Quotas)
	}
	s.auditOp(id, t.ID, "tenant.update", t.ID, detail)
	writeJSON(w, t)
}

func (s *Server) suspendTenant(suspend bool) handler {
	return func(w http.ResponseWriter, r *http.Request, id edition.Identity) {
		var in struct {
			Reason string `json:"reason"`
		}
		if suspend && !readJSON(w, r, &in) {
			return
		}
		status, action := tenants.Active, "tenant.resume"
		if suspend {
			status, action = tenants.Suspended, "tenant.suspend"
		}
		t, err := s.ten.Tenants.SetStatus(r.PathValue("id"), status, in.Reason)
		if err != nil {
			s.tenantErr(w, err)
			return
		}
		s.auditOp(id, t.ID, action, t.ID, in.Reason)
		writeJSON(w, t)
	}
}

func (s *Server) offboardTenant(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in struct {
		Confirm string `json:"confirm"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if s.ten.Off == nil {
		writeErr(w, http.StatusServiceUnavailable, "removing tenants is not available on this server")
		return
	}
	tid := r.PathValue("id")
	if err := s.ten.Off.Start(tid, in.Confirm); err != nil {
		s.tenantErr(w, err)
		return
	}
	s.auditOp(id, tid, "tenant.offboard", tid, "removal started")
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]any{"ok": true, "note": "The removal runs in the background. Open the tenant to follow it."})
}

func (s *Server) exportTenant(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	tid := r.PathValue("id")
	if t, ok := s.ten.Tenants.Get(tid); !ok || t.Status == tenants.Deleted {
		s.tenantErr(w, tenants.ErrNotFound)
		return
	}
	s.auditOp(id, tid, "tenant.export", tid, "")
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="lumen-tenant-%s-%s.zip"`, safeFile(tid), time.Now().UTC().Format("20060102")))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := s.ten.Tenants.Export(w, tid); err != nil {
		s.log.Error("export failed", "tenant", tid, "err", err)
	}
}

var unsafeFile = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

func safeFile(s string) string { return unsafeFile.ReplaceAllString(s, "_") }

func (s *Server) enterTenant(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in struct {
		Write   bool `json:"write"`
		Minutes int  `json:"minutes"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	tid := r.PathValue("id")
	if tid == perm.OperatorTenant {
		writeErr(w, http.StatusBadRequest, "you are already in the operator tenant")
		return
	}
	if err := s.ten.Tenants.CanEnter(tid); err != nil {
		s.tenantErr(w, err)
		return
	}
	u, ok := s.a.Store.GetUser(id.User)
	if !ok || u.Tenant != perm.OperatorTenant {
		writeErr(w, http.StatusForbidden, "only a user of the operator tenant can go into a tenant")
		return
	}
	if in.Minutes <= 0 {
		in.Minutes = 60
	}
	tok, until := s.a.IssueActing(u, tid, time.Duration(in.Minutes)*time.Minute, in.Write)
	auth.SetCookie(w, r, tok, int(auth.SessionTTL.Seconds()))
	mode := "read-only"
	if in.Write {
		mode = "with write access"
	}
	s.auditOp(id, tid, "support.enter", tid, fmt.Sprintf("%s, until %s", mode, until.UTC().Format(time.RFC3339)))
	writeJSON(w, map[string]any{"ok": true, "tenant": tid, "until": until.UTC().Format(time.RFC3339), "write": in.Write})
}

func (s *Server) leaveTenant(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if !id.Acting {
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	u, ok := s.a.Store.GetUser(id.Operator)
	if !ok {
		auth.SetCookie(w, r, "", -1)
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	auth.SetCookie(w, r, s.a.IssueSession(u), int(auth.SessionTTL.Seconds()))
	s.auditOp(edition.Identity{User: id.Operator}, id.Tenant, "support.leave", id.Tenant, "")
	writeJSON(w, map[string]any{"ok": true})
}

var dayRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func (s *Server) usageRange(w http.ResponseWriter, r *http.Request) (from, to string, ok bool) {
	now := time.Now().UTC()
	from, to = r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if from == "" {
		from = now.AddDate(0, 0, -29).Format("2006-01-02")
	}
	if to == "" {
		to = now.Format("2006-01-02")
	}
	if !dayRe.MatchString(from) || !dayRe.MatchString(to) || from > to {
		writeErr(w, http.StatusBadRequest, "from and to must be days like 2026-10-31, and from must not be after to")
		return "", "", false
	}
	return from, to, true
}

func (s *Server) operatorUsage(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	from, to, ok := s.usageRange(w, r)
	if !ok {
		return
	}
	days := s.ten.Meter.Range(r.URL.Query().Get("tenant"), from, to)
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="lumen-usage-%s-%s.csv"`, from, to))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write([]byte(metering.CSV(days)))
		return
	}
	if days == nil {
		days = []metering.Day{}
	}
	writeJSON(w, map[string]any{"data": days, "from": from, "to": to})
}

func (s *Server) operatorAudit(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	writeJSON(w, map[string]any{"data": s.ten.Audit.List(r.URL.Query().Get("tenant"), 300)})
}

// ---- what a tenant sees of itself ----

func (s *Server) ownUsage(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	t, ok := s.ten.Tenants.Get(id.Tenant)
	if !ok {
		s.touch(id.Tenant)
		t, _ = s.ten.Tenants.Get(id.Tenant)
	}
	u, warn := s.usageOf(t)
	now := time.Now().UTC()
	days := s.ten.Meter.Range(id.Tenant, now.AddDate(0, 0, -29).Format("2006-01-02"), now.Format("2006-01-02"))
	if days == nil {
		days = []metering.Day{}
	}
	// the limits are shown as they apply: with no Operator licence nothing is enforced
	writeJSON(w, map[string]any{"quotas": t.Quotas, "enforced": s.enforced(), "status": t.Status, "usage": u, "warnings": warn, "days": days})
}

func (s *Server) getSupport(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	t, _ := s.ten.Tenants.Get(id.Tenant)
	var grant *tenants.Grant
	if g, ok := s.ten.Tenants.ActiveGrant(id.Tenant); ok {
		grant = &g
	}
	writeJSON(w, map[string]any{"mode": t.SupportAccess, "grant": grant})
}

func (s *Server) putSupport(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in struct {
		Mode string `json:"mode"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if id.Acting { // an operator cannot give itself access
		writeErr(w, http.StatusForbidden, "the support-access setting is the tenant's own decision")
		return
	}
	t, err := s.ten.Tenants.SetSupportAccess(id.Tenant, in.Mode)
	if err != nil {
		s.tenantErr(w, err)
		return
	}
	s.auditOp(id, id.Tenant, "support.setting", t.SupportAccess, "")
	s.getSupport(w, r, id)
}

func (s *Server) grantSupport(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in struct {
		Minutes int `json:"minutes"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if id.Acting {
		writeErr(w, http.StatusForbidden, "the support-access setting is the tenant's own decision")
		return
	}
	g, err := s.ten.Tenants.GrantAccess(id.Tenant, id.User, time.Duration(in.Minutes)*time.Minute)
	if err != nil {
		s.tenantErr(w, err)
		return
	}
	s.auditOp(id, id.Tenant, "support.grant", id.Tenant, "until "+g.Until.Format(time.RFC3339))
	s.getSupport(w, r, id)
}

func (s *Server) revokeSupport(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if id.Acting {
		writeErr(w, http.StatusForbidden, "the support-access setting is the tenant's own decision")
		return
	}
	s.ten.Tenants.Revoke(id.Tenant)
	s.auditOp(id, id.Tenant, "support.revoke", id.Tenant, "")
	s.getSupport(w, r, id)
}

func (s *Server) ownAudit(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	out := s.ten.Audit.List(id.Tenant, 200)
	if out == nil {
		out = []audit.Entry{}
	}
	writeJSON(w, map[string]any{"data": out})
}

var _ = license.Operator
