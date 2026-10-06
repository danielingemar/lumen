package server

import (
	"net/http"

	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/perm"
	"github.com/danielingemar/lumen/internal/store"
)

type handler = func(http.ResponseWriter, *http.Request, edition.Identity)

// perms returns the identity's permissions. An identity from a custom authenticator that did not set any is
// treated like an API key (agents and scripts) and never like an admin.
func perms(id edition.Identity) edition.Identity {
	if id.Perms == nil && !id.Session {
		id.Perms = perm.Key()
	}
	return id
}

func (s *Server) identity(w http.ResponseWriter, r *http.Request) (edition.Identity, bool) {
	id, ok := s.who(w, r)
	if !ok || !s.tenantUsable(w, id) {
		return id, false
	}
	return id, true
}

// who resolves the caller without asking whether the tenant may be used (ingest answers that itself: a suspended tenant's
// data is refused or, if the operator chose so, silently dropped).
func (s *Server) who(w http.ResponseWriter, r *http.Request) (edition.Identity, bool) {
	id, err := s.auth.Authenticate(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid or missing credentials")
		return id, false
	}
	return perms(id), true
}

// authed is for endpoints that need no area permission: "ingest" (API keys only, never a browser session)
// and "any" (any signed-in identity).
func (s *Server) authed(action string, h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := s.identity
		if action == "ingest" {
			who = s.who
		}
		id, ok := who(w, r)
		if !ok {
			return
		}
		if action == "ingest" && id.Session {
			writeErr(w, http.StatusForbidden, "sending telemetry requires an API key, not a browser login")
			return
		}
		if !edition.Authz.Allow(id, action) {
			writeErr(w, http.StatusForbidden, "forbidden")
			return
		}
		h(w, r, id)
	})
}

func level(write bool) string {
	if write {
		return "write"
	}
	return "read"
}

// withArchive lets read requests ask for archived data (?archive=1). That needs the backups permission.
func withArchive(w http.ResponseWriter, r *http.Request, id edition.Identity) (*http.Request, bool) {
	if r.URL.Query().Get("archive") != "1" {
		return r, true
	}
	if !id.Can(perm.Backups, false) {
		writeErr(w, http.StatusForbidden, "you do not have access to the archive")
		return r, false
	}
	return r.WithContext(store.WithArchive(r.Context())), true
}

// need requires read or write permission on one area.
func (s *Server) need(area string, write bool, h handler) http.Handler {
	return s.gate([]string{area}, write, h)
}

// needAny requires read permission on at least one of the areas (the handler narrows further if needed).
func (s *Server) needAny(areas []string, h handler) http.Handler { return s.gate(areas, false, h) }

func (s *Server) gate(areas []string, write bool, h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.identity(w, r)
		if !ok {
			return
		}
		allowed := false
		for _, a := range areas {
			if id.Can(a, write) && edition.Authz.Allow(id, a+":"+level(write)) {
				allowed = true
				break
			}
		}
		if !allowed {
			writeErr(w, http.StatusForbidden, "your account does not have "+level(write)+" access to this ("+areas[0]+"). Ask an administrator to change your group.")
			return
		}
		if !write {
			var ok bool
			if r, ok = withArchive(w, r, id); !ok {
				return
			}
		}
		s.auditActing(r, id) // an operator inside a tenant: everything that changes something is on record
		h(w, r, id)
	})
}

// sessionOnly requires a username login (not an API key or API-key session), and, when area is set, that permission.
// Agent keys must not be able to mint keys or change passwords.
func (s *Server) sessionOnly(area string, write bool, h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := s.auth.Authenticate(r)
		if err != nil || !id.Session {
			writeErr(w, http.StatusUnauthorized, "please log in")
			return
		}
		id = perms(id)
		if !s.tenantUsable(w, id) {
			return
		}
		if id.ViaKey {
			writeErr(w, http.StatusForbidden, "this needs a username and password login; an API key session cannot manage keys, users or passwords")
			return
		}
		if area != "" && !id.Can(area, write) {
			writeErr(w, http.StatusForbidden, "your account does not have "+level(write)+" access to this ("+area+")")
			return
		}
		if !edition.Authz.Allow(id, "manage") {
			writeErr(w, http.StatusForbidden, "forbidden")
			return
		}
		s.auditActing(r, id) // changes made by an operator inside a tenant are on record, also those that need a real login
		h(w, r, id)
	})
}
