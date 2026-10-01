package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/danielingemar/lumen/internal/backup"
	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/perm"
)

// BackupInfo is shown in the UI so people know what is configured.
type BackupInfo struct {
	DataDays int `json:"data_retention_days"`
	KeepDays int `json:"backup_retention_days"`
}

// WithBackups enables the backup and archive API.
func (s *Server) WithBackups(m *backup.Manager, info BackupInfo) *Server {
	s.bk, s.bkInfo = m, info
	return s
}

func (s *Server) backupRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/backups", s.need(perm.Backups, false, s.listBackups))
	if s.bk == nil {
		return
	}
	mux.Handle("POST /api/v1/backups/run", s.need(perm.Backups, true, s.runBackup))
	mux.Handle("POST /api/v1/backups/{day}/load", s.need(perm.Backups, true, s.loadBackup))
	mux.Handle("DELETE /api/v1/backups/{day}/load", s.need(perm.Backups, true, s.unloadBackup))
	mux.Handle("GET /api/v1/backups/{day}/{table}", s.need(perm.Backups, false, s.downloadBackup))
}

func (s *Server) backupErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, backup.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, backup.ErrNotFound):
		writeErr(w, http.StatusNotFound, "there is no backup of that day")
	case errors.Is(err, backup.ErrBusy):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		s.log.Error("backup operation failed", "err", err)
		writeErr(w, http.StatusBadGateway, "the operation failed: "+err.Error())
	}
}

func (s *Server) listBackups(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if s.bk == nil {
		writeJSON(w, map[string]any{"enabled": false, "days": []any{}})
		return
	}
	loaded := map[string]bool{}
	if days, err := s.bk.Loaded(r.Context(), id.Tenant); err == nil {
		for _, d := range days {
			loaded[d] = true
		}
	}
	type dayOut struct {
		backup.Day
		Loaded bool `json:"loaded"`
	}
	out := []dayOut{}
	for _, d := range s.bk.List(id.Tenant) {
		out = append(out, dayOut{d, loaded[d.Day]})
	}
	writeJSON(w, map[string]any{"enabled": true, "days": out, "state": s.bk.State(), "info": s.bkInfo})
}

func (s *Server) runBackup(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if s.bk.State().Running {
		writeErr(w, http.StatusConflict, backup.ErrBusy.Error())
		return
	}
	go func() { // a first run can export a month of data; do not tie it to this request
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		if err := s.bk.RunOnce(ctx); err != nil {
			s.log.Error("backup failed", "err", err)
		}
	}()
	s.log.Info("backup started by user", "user", id.User, "tenant", id.Tenant)
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]bool{"started": true})
}

func (s *Server) loadBackup(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	if err := s.bk.Load(ctx, id.Tenant, r.PathValue("day")); err != nil {
		s.backupErr(w, err)
		return
	}
	s.log.Info("archive day loaded", "user", id.User, "tenant", id.Tenant, "day", r.PathValue("day"))
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) unloadBackup(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	if err := s.bk.Unload(ctx, id.Tenant, r.PathValue("day")); err != nil {
		s.backupErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	path, err := s.bk.File(id.Tenant, r.PathValue("day"), r.PathValue("table"))
	if err != nil {
		s.backupErr(w, err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		s.backupErr(w, backup.ErrNotFound)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="lumen-`+id.Tenant+`-`+r.PathValue("day")+`-`+r.PathValue("table")+`.jsonl.gz"`)
	http.ServeContent(w, r, "", st.ModTime(), f)
}
