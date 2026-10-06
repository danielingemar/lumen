package server

import (
	"errors"
	"net/http"

	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/license"
	"github.com/danielingemar/lumen/internal/perm"
)

// WithLicense enables the licence settings and tells the interface what the licence allows.
func (s *Server) WithLicense(m *license.Manager) *Server { s.lic = m; return s }

// WithOwnerTenant says which tenant owns the installation. The licence belongs to the installation, not to one customer,
// so in a multi-tenant installation only users of this tenant may see or change it. (Until the Operator add-on gives
// an operator role of its own, this is the tenant that was created when Lumen was installed.)
func (s *Server) WithOwnerTenant(t string) *Server { s.owner = t; return s }

func (s *Server) isOwner(id edition.Identity) bool { return s.owner == "" || id.Tenant == s.owner }

func (s *Server) licenseRoutes(mux *http.ServeMux) {
	if s.lic == nil {
		return
	}
	mux.Handle("GET /api/v1/settings/license", s.need(perm.Settings, false, s.getLicense))
	mux.Handle("PUT /api/v1/settings/license", s.need(perm.Settings, true, s.putLicense))
	mux.Handle("DELETE /api/v1/settings/license", s.need(perm.Settings, true, s.deleteLicense))
}

func (s *Server) licenseView(r *http.Request, id edition.Identity) map[string]any {
	hosts := 0
	if sum, err := s.Summary(r.Context(), id.Tenant); err == nil {
		hosts = len(sum.HostList)
	}
	w := s.lic.Warnings(hosts, 0)
	if w == nil {
		w = []string{}
	}
	// the host count is that of the signed-in tenant: an installation-wide count needs the tenant records of the Operator add-on
	return map[string]any{"license": s.lic.Info(), "usage": map[string]any{"hosts": hosts, "scope": "tenant"}, "warnings": w}
}

func (s *Server) getLicense(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if !s.isOwner(id) {
		writeErr(w, http.StatusForbidden, "the licence belongs to the installation and is managed from the tenant that owns it ("+s.owner+")")
		return
	}
	writeJSON(w, s.licenseView(r, id))
}

func (s *Server) putLicense(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if !s.isOwner(id) {
		writeErr(w, http.StatusForbidden, "the licence belongs to the installation and is managed from the tenant that owns it ("+s.owner+")")
		return
	}
	var in struct {
		License string `json:"license"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	l, err := s.lic.Set([]byte(in.License), id.User)
	if err != nil {
		if errors.Is(err, license.ErrInvalid) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.log.Error("licence could not be saved", "err", err)
		writeErr(w, http.StatusInternalServerError, "the licence could not be saved")
		return
	}
	s.auditOp(id, id.Tenant, "licence.install", l.ID, l.Customer)
	s.log.Info("licence installed", "by", id.User, "customer", l.Customer, "id", l.ID, "editions", l.Editions, "expires", l.Expires.Format("2006-01-02"))
	writeJSON(w, s.licenseView(r, id))
}

func (s *Server) deleteLicense(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if !s.isOwner(id) {
		writeErr(w, http.StatusForbidden, "the licence belongs to the installation and is managed from the tenant that owns it ("+s.owner+")")
		return
	}
	if err := s.lic.Remove(); err != nil {
		if errors.Is(err, license.ErrInvalid) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "the licence could not be removed")
		return
	}
	s.auditOp(id, id.Tenant, "licence.remove", "", "")
	s.log.Info("licence removed", "by", id.User)
	writeJSON(w, s.licenseView(r, id))
}
