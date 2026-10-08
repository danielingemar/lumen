package server

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/danielingemar/lumen/internal/backup"
	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/health"
	"github.com/danielingemar/lumen/internal/perm"
)

// BackupWhere is what the setting "where backups are kept" needs.
type BackupWhere struct {
	DB      docstore.Backend
	Where   *backup.Where
	Default string   // the folder when nobody has chosen one (LUMEN_BACKUP_DIR)
	Roots   []string // where a folder may be (LUMEN_BACKUP_ROOTS)
	DataDir string
}

func (s *Server) WithBackupLocation(b *BackupWhere) *Server { s.backupWhere = b; return s }

func (s *Server) backupLocationRoutes(mux *http.ServeMux) {
	if s.backupWhere == nil {
		return
	}
	mux.Handle("GET /api/v1/backup-location", s.need(perm.Backups, false, s.getBackupLocation))
	mux.Handle("POST /api/v1/backup-location/check", s.need(perm.Backups, true, s.checkBackupLocation))
	mux.Handle("PUT /api/v1/backup-location", s.need(perm.Backups, true, s.putBackupLocation))
}

var dayDirRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// backupDays counts the days of backups in a folder.
func backupDays(dir string) int {
	entries, _ := os.ReadDir(filepath.Join(dir, "telemetry"))
	n := 0
	for _, e := range entries {
		if e.IsDir() && dayDirRe.MatchString(e.Name()) {
			n++
		}
	}
	return n
}

// ownerOnly: where the backups of the installation go is for whoever runs it.
func (s *Server) ownerOnly(w http.ResponseWriter, id edition.Identity) bool {
	if s.ownsInstallation(id.Tenant) {
		return true
	}
	writeErr(w, http.StatusForbidden, "where the backups are kept is set by the tenant that runs the installation ("+s.owner+")")
	return false
}

func (s *Server) getBackupLocation(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if !s.ownerOnly(w, id) {
		return
	}
	b := s.backupWhere
	dir := b.Where.Get()
	source := "default"
	if b.Where.Strict() {
		source = "chosen"
	}
	writeJSON(w, map[string]any{"dir": dir, "source": source, "default": b.Default, "roots": rootsOrDefault(b.Roots),
		"info": backup.Check(dir, b.Roots, b.DataDir, false, health.Statfs), "days": backupDays(dir)})
}

func rootsOrDefault(r []string) []string {
	if len(r) == 0 {
		return backup.DefaultRoots
	}
	return r
}

// checkBackupLocation looks at a folder without saving it: is it there, can it be written to, how much room is there.
func (s *Server) checkBackupLocation(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if !s.ownerOnly(w, id) {
		return
	}
	var in struct {
		Dir string `json:"dir"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	b := s.backupWhere
	writeJSON(w, map[string]any{"info": backup.Check(in.Dir, b.Roots, b.DataDir, true, health.Statfs), "days": backupDays(in.Dir)})
}

// putBackupLocation chooses the folder (or, with an empty one, goes back to the default). It is used from the next backup;
// backups that are already made stay where they are.
func (s *Server) putBackupLocation(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if !s.ownerOnly(w, id) {
		return
	}
	var in struct {
		Dir string `json:"dir"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	b := s.backupWhere
	previous := b.Where.Get()
	if in.Dir == "" {
		if err := backup.Save(r.Context(), b.DB, "", id.User); err != nil {
			writeErr(w, http.StatusServiceUnavailable, "could not save the setting")
			return
		}
		b.Where.Set(b.Default, false)
	} else {
		info := backup.Check(in.Dir, b.Roots, b.DataDir, true, health.Statfs)
		if info.Problem != "" {
			writeErr(w, http.StatusBadRequest, info.Problem)
			return
		}
		if err := backup.Save(r.Context(), b.DB, in.Dir, id.User); err != nil {
			writeErr(w, http.StatusServiceUnavailable, "could not save the setting")
			return
		}
		b.Where.Set(in.Dir, true)
	}
	now := b.Where.Get()
	s.auditOp(id, id.Tenant, "backup.location", now, "from "+previous)
	writeJSON(w, map[string]any{"ok": true, "dir": now, "previous": previous, "previous_days": backupDays(previous)})
}
