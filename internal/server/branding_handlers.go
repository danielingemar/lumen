package server

import (
	"errors"
	"net/http"

	"github.com/danielingemar/lumen/internal/branding"
	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/perm"
)

// WithBranding enables the site name and logo.
func (s *Server) WithBranding(b *branding.Service) *Server { s.brand = b; return s }

func (s *Server) brandingRoutes(mux *http.ServeMux) {
	if s.brand == nil {
		return
	}
	// public: the login page needs the logo before anyone is signed in
	mux.HandleFunc("GET /api/v1/branding", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		writeJSON(w, s.brand.Public())
	})
	mux.HandleFunc("GET /branding/logo", func(w http.ResponseWriter, r *http.Request) {
		raw, mime, ok := s.brand.Logo()
		if !ok {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", mime)
		// A logo is an untrusted upload served from our own origin: no script may run even if someone opens it directly.
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "public, max-age=86400") // the URL carries ?v=, which changes with the logo
		w.Write(raw)
	})
	mux.Handle("PUT /api/v1/settings/branding", s.need(perm.Settings, true, s.putBranding))
}

func (s *Server) putBranding(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in struct {
		Name       string `json:"name"`
		Logo       string `json:"logo"` // data URL; empty keeps the current logo
		RemoveLogo bool   `json:"remove_logo"`
	}
	if !readJSONLimit(w, r, &in, 1<<20) {
		return
	}
	p, err := s.brand.Update(in.Name, in.Logo, in.RemoveLogo)
	switch {
	case errors.Is(err, branding.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	case err != nil:
		s.log.Error("branding update failed", "err", err)
		writeErr(w, http.StatusServiceUnavailable, "the settings store is unavailable")
	default:
		s.log.Info("branding changed", "by", id.User, "tenant", id.Tenant)
		writeJSON(w, p)
	}
}
