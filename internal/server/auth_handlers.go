package server

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"

	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/perm"
)

// WithAuth enables username/password login, sessions and stored API keys.
func (s *Server) WithAuth(a *auth.Auth) *Server {
	s.a, s.auth = a, a
	return s
}

func (s *Server) authRoutes(mux *http.ServeMux) {
	if s.a == nil {
		return
	}
	mux.HandleFunc("POST /api/v1/login", s.login)
	mux.HandleFunc("POST /api/v1/logout", s.logout)
	mux.Handle("GET /api/v1/me", s.authed("any", s.me))
	mux.Handle("GET /api/v1/keys", s.sessionOnly(perm.Keys, false, s.listKeys))
	mux.Handle("POST /api/v1/keys", s.sessionOnly(perm.Keys, true, s.createKey))
	mux.Handle("DELETE /api/v1/keys/{id}", s.sessionOnly(perm.Keys, true, s.deleteKey))
	mux.Handle("POST /api/v1/password", s.sessionOnly("", false, s.changePassword))
}

// readJSON enforces a JSON content type (a browser cannot send that cross-site without a preflight,
// which, together with SameSite=Strict cookies, blocks cross-site request forgery) and a small body.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return readJSONLimit(w, r, v, 8<<10)
}

func readJSONLimit(w http.ResponseWriter, r *http.Request, v any, max int64) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeErr(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, max)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func remoteHost(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		APIKey   string `json:"api_key"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.APIKey != "" { // sign in with an API key instead of a username and password
		bucket := "key:" + remoteHost(r)
		if s.a.Limiter.Blocked(bucket) {
			writeErr(w, http.StatusTooManyRequests, "too many failed attempts, try again in a few minutes")
			return
		}
		tok, tenant, ok := s.a.LoginWithKey(strings.TrimSpace(in.APIKey))
		if !ok {
			s.a.Limiter.Fail(bucket)
			s.log.Warn("failed API key login", "remote", r.RemoteAddr)
			writeErr(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		s.a.Limiter.Reset(bucket)
		auth.SetCookie(w, r, tok, int(auth.SessionTTL.Seconds()))
		writeJSON(w, map[string]any{"user": "", "tenant": tenant, "via_key": true})
		return
	}
	name, err := auth.NormalizeUser(in.Username)
	if err != nil {
		name = "invalid"
	}
	if s.a.Limiter.Blocked(name) {
		writeErr(w, http.StatusTooManyRequests, "too many failed attempts, try again in a few minutes")
		return
	}
	u, ok := s.a.Store.VerifyLogin(name, in.Password)
	if !ok {
		s.a.Limiter.Fail(name)
		s.log.Warn("failed login", "user", name, "remote", r.RemoteAddr)
		writeErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	s.a.Limiter.Reset(name)
	auth.SetCookie(w, r, s.a.IssueSession(u), int(auth.SessionTTL.Seconds()))
	writeJSON(w, map[string]any{"user": u.Name, "tenant": u.Tenant})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	auth.SetCookie(w, r, "", -1)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	group, groupName := "", "API key"
	if id.User != "" {
		if u, ok := s.a.Store.GetUser(id.User); ok {
			group = auth.EffectiveGroup(u)
			if g, ok := s.a.Store.GetGroup(u.Tenant, group); ok {
				groupName = g.Name
			}
		}
	} else if !id.Session {
		groupName = "Administrator (dev mode)"
	}
	writeJSON(w, map[string]any{"user": id.User, "tenant": id.Tenant, "session": id.Session, "via_key": id.ViaKey, "group": group, "group_name": groupName, "perms": id.Perms, "archive_enabled": id.Can(perm.Backups, false)})
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	keys := s.a.Store.ListKeys(id.Tenant)
	if keys == nil {
		keys = []auth.APIKey{}
	}
	writeJSON(w, map[string]any{"data": keys})
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in struct{ Name string }
	if !readJSON(w, r, &in) {
		return
	}
	plain, k, err := s.a.Store.CreateKey(id.Tenant, in.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not create key")
		return
	}
	s.log.Info("api key created", "user", id.User, "tenant", id.Tenant, "id", k.ID)
	writeJSON(w, map[string]any{"key": plain, "id": k.ID, "name": k.Name, "prefix": k.Prefix})
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if err := s.a.Store.DeleteKey(id.Tenant, r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, "no such key")
		return
	}
	s.log.Info("api key deleted", "user", id.User, "tenant", id.Tenant, "id", r.PathValue("id"))
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in struct{ Current, New string }
	if !readJSON(w, r, &in) {
		return
	}
	if _, ok := s.a.Store.VerifyLogin(id.User, in.Current); !ok {
		writeErr(w, http.StatusUnauthorized, "current password is wrong")
		return
	}
	if err := s.a.Store.SetPassword(id.User, in.New); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// all existing sessions (including this one) end because the password fingerprint changed
	auth.SetCookie(w, r, "", -1)
	writeJSON(w, map[string]bool{"ok": true})
}
