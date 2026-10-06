package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/danielingemar/lumen/internal/dashboards"
	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/perm"
)

// WithDashboards enables the dashboard CRUD API.
func (s *Server) WithDashboards(d *dashboards.Service) *Server { s.dash = d; return s }

func (s *Server) dashRoutes(mux *http.ServeMux) {
	if s.dash == nil {
		return
	}
	mux.Handle("GET /api/v1/dashboards", s.need(perm.Dashboards, false, s.listDash))
	mux.Handle("POST /api/v1/dashboards", s.need(perm.Dashboards, true, s.createDash))
	mux.Handle("GET /api/v1/dashboards/{id}", s.need(perm.Dashboards, false, s.getDash))
	mux.Handle("PUT /api/v1/dashboards/{id}", s.need(perm.Dashboards, true, s.updateDash))
	mux.Handle("DELETE /api/v1/dashboards/{id}", s.need(perm.Dashboards, true, s.deleteDash))
}

type dashIn struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Body        json.RawMessage `json:"body"`
	Version     string          `json:"version"`
}

func (s *Server) dashErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, dashboards.ErrNotFound):
		writeErr(w, http.StatusNotFound, "dashboard not found")
	case errors.Is(err, dashboards.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, dashboards.ErrConflict):
		writeErr(w, http.StatusConflict, "this dashboard was changed by someone else; reload it and apply your change again")
	default:
		s.log.Error("dashboard store error", "err", err)
		writeErr(w, http.StatusServiceUnavailable, "the dashboard store is unavailable")
	}
}

func (s *Server) listDash(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	d, err := s.dash.List(id.Tenant)
	if err != nil {
		s.dashErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"data": d})
}

func (s *Server) getDash(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	d, err := s.dash.Get(id.Tenant, r.PathValue("id"))
	if err != nil {
		s.dashErr(w, err)
		return
	}
	writeJSON(w, d)
}

func (s *Server) createDash(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if !s.quotaOK(w, id, "dashboards") {
		return
	}
	var in dashIn
	if !readJSONLimit(w, r, &in, dashboards.MaxBody+4096) {
		return
	}
	d, err := s.dash.Create(id.Tenant, id.User, in.Name, in.Description, in.Body)
	if err != nil {
		s.dashErr(w, err)
		return
	}
	writeJSON(w, d)
}

func (s *Server) updateDash(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in dashIn
	if !readJSONLimit(w, r, &in, dashboards.MaxBody+4096) {
		return
	}
	d, err := s.dash.Update(id.Tenant, r.PathValue("id"), in.Name, in.Description, in.Body, in.Version)
	if err != nil {
		s.dashErr(w, err)
		return
	}
	writeJSON(w, d)
}

func (s *Server) deleteDash(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if err := s.dash.Delete(id.Tenant, r.PathValue("id")); err != nil {
		s.dashErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
