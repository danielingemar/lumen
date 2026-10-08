package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/health"
	"github.com/danielingemar/lumen/internal/perm"
)

// WithHealth lets Lumen look at itself: the disk it writes to, Elasticsearch and ClickHouse.
func (s *Server) WithHealth(m *health.Monitor) *Server { s.health = m; return s }

func (s *Server) healthRoutes(mux *http.ServeMux) {
	if s.health == nil {
		return
	}
	mux.Handle("GET /api/v1/health", s.need(perm.Settings, false, s.getHealth))
}

// ownsInstallation: the health of the installation is for whoever runs it, not for the customers of a multi-tenant installation.
func (s *Server) ownsInstallation(tenant string) bool { return s.owner == "" || tenant == s.owner }

func (s *Server) getHealth(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if !s.ownsInstallation(id.Tenant) {
		writeErr(w, http.StatusForbidden, "the health of the installation is for the tenant that runs it ("+s.owner+")")
		return
	}
	var rep health.Report
	if r.URL.Query().Get("refresh") == "1" {
		rep = s.health.Refresh(r.Context())
	} else {
		rep = s.health.Report(r.Context())
	}
	writeJSON(w, map[string]any{"report": rep, "warn": s.health.Warn, "crit": s.health.Crit})
}

// Health is what the alert rules about Lumen itself read. Anyone but the owner gets an empty report.
func (s *Server) Health(ctx context.Context, tenant string) (health.Report, error) {
	if s.health == nil || !s.ownsInstallation(tenant) {
		return health.Report{}, nil
	}
	return s.health.Report(ctx), nil
}

// lumenRule tells whether a rule is about Lumen itself.
func lumenRule(status string) bool { return strings.HasPrefix(status, "lumen_") }
