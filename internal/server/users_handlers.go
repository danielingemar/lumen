package server

import (
	"net/http"
	"strings"

	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/perm"
)

func (s *Server) userRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/users", s.sessionOnly(perm.Users, false, s.listUsers))
	mux.Handle("POST /api/v1/users", s.sessionOnly(perm.Users, true, s.createUser))
	mux.Handle("PUT /api/v1/users/{name}", s.sessionOnly(perm.Users, true, s.updateUser))
	mux.Handle("DELETE /api/v1/users/{name}", s.sessionOnly(perm.Users, true, s.deleteUser))
	mux.Handle("GET /api/v1/groups", s.sessionOnly(perm.Users, false, s.listGroups))
	mux.Handle("POST /api/v1/groups", s.sessionOnly(perm.Users, true, s.createGroup))
	mux.Handle("PUT /api/v1/groups/{id}", s.sessionOnly(perm.Users, true, s.updateGroup))
	mux.Handle("DELETE /api/v1/groups/{id}", s.sessionOnly(perm.Users, true, s.deleteGroup))
}

type userOut struct {
	Name      string `json:"name"`
	Group     string `json:"group"`
	GroupName string `json:"group_name"`
	Summary   string `json:"summary"`
	Created   string `json:"created"`
}

func (s *Server) userOut(u auth.User) userOut {
	g, ok := s.a.Store.GetGroup(u.Tenant, auth.EffectiveGroup(u))
	name, sum := "(deleted group)", "no access"
	if ok {
		name, sum = g.Name, perm.Summary(g.Perms)
	}
	return userOut{Name: u.Name, Group: auth.EffectiveGroup(u), GroupName: name, Summary: sum, Created: u.Created.Format("2006-01-02")}
}

// sameTenantUser finds a user of the caller's tenant; other tenants' users look like they do not exist.
func (s *Server) sameTenantUser(w http.ResponseWriter, id edition.Identity, name string) (auth.User, bool) {
	u, ok := s.a.Store.GetUser(name)
	if !ok || u.Tenant != id.Tenant {
		writeErr(w, http.StatusNotFound, "no such user")
		return u, false
	}
	return u, true
}

// otherAdmins counts users of the tenant, except one, who can manage users. The last one must never be removed.
func (s *Server) otherAdmins(tenant, except string) int {
	n := 0
	for _, u := range s.a.Store.ListUsersIn(tenant) {
		if u.Name != except && s.a.Store.PermsFor(u)[perm.Users] == perm.Write {
			n++
		}
	}
	return n
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	out := []userOut{}
	for _, u := range s.a.Store.ListUsersIn(id.Tenant) {
		out = append(out, s.userOut(u))
	}
	writeJSON(w, map[string]any{"data": out})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in struct{ Name, Password, Group string }
	if !readJSON(w, r, &in) {
		return
	}
	if in.Group == "" {
		in.Group = "user"
	}
	generated := ""
	if in.Password == "" {
		generated = auth.RandomToken(9)
		in.Password = generated
	}
	if err := s.a.Store.CreateUserIn(in.Name, id.Tenant, in.Password, in.Group); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("user created", "by", id.User, "tenant", id.Tenant, "user", strings.ToLower(in.Name), "group", in.Group)
	u, _ := s.a.Store.GetUser(in.Name)
	res := map[string]any{"user": s.userOut(u)}
	if generated != "" {
		res["password"] = generated // shown once
	}
	writeJSON(w, res)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in struct{ Group, Password string }
	if !readJSON(w, r, &in) {
		return
	}
	u, ok := s.sameTenantUser(w, id, r.PathValue("name"))
	if !ok {
		return
	}
	self := u.Name == id.User
	res := map[string]any{}
	if in.Group != "" && in.Group != auth.EffectiveGroup(u) {
		if self {
			writeErr(w, http.StatusBadRequest, "you cannot change your own group; ask another administrator")
			return
		}
		g, ok := s.a.Store.GetGroup(id.Tenant, in.Group)
		if !ok {
			writeErr(w, http.StatusBadRequest, "no such group")
			return
		}
		if g.Perms[perm.Users] != perm.Write && s.otherAdmins(id.Tenant, u.Name) == 0 {
			writeErr(w, http.StatusBadRequest, "at least one user must be able to manage users")
			return
		}
		if err := s.a.Store.SetGroup(u.Name, in.Group); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if in.Password == "reset" { // generate a new password; the old sessions end
		if self {
			writeErr(w, http.StatusBadRequest, "change your own password under Account")
			return
		}
		pw := auth.RandomToken(9)
		if err := s.a.Store.SetPassword(u.Name, pw); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		res["password"] = pw
	}
	s.log.Info("user updated", "by", id.User, "tenant", id.Tenant, "user", u.Name)
	nu, _ := s.a.Store.GetUser(u.Name)
	res["user"] = s.userOut(nu)
	writeJSON(w, res)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	u, ok := s.sameTenantUser(w, id, r.PathValue("name"))
	if !ok {
		return
	}
	if u.Name == id.User {
		writeErr(w, http.StatusBadRequest, "you cannot delete yourself")
		return
	}
	if s.a.Store.PermsFor(u)[perm.Users] == perm.Write && s.otherAdmins(id.Tenant, u.Name) == 0 {
		writeErr(w, http.StatusBadRequest, "this is the last user who can manage users")
		return
	}
	if err := s.a.Store.DeleteUser(u.Name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("user deleted", "by", id.User, "tenant", id.Tenant, "user", u.Name)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	members := map[string]int{}
	for _, u := range s.a.Store.ListUsersIn(id.Tenant) {
		members[auth.EffectiveGroup(u)]++
	}
	type gOut struct {
		auth.Group
		Members int    `json:"members"`
		Summary string `json:"summary"`
	}
	out := []gOut{}
	for _, g := range s.a.Store.ListGroups(id.Tenant) {
		out = append(out, gOut{g, members[g.ID], perm.Summary(g.Perms)})
	}
	writeJSON(w, map[string]any{"data": out, "areas": perm.Areas})
}

type groupIn struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Perms       map[string]string `json:"perms"`
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in groupIn
	if !readJSON(w, r, &in) {
		return
	}
	g, err := s.a.Store.CreateGroup(id.Tenant, in.Name, in.Description, in.Perms)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("group created", "by", id.User, "tenant", id.Tenant, "group", g.Name)
	writeJSON(w, g)
}

func (s *Server) updateGroup(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in groupIn
	if !readJSON(w, r, &in) {
		return
	}
	gid := r.PathValue("id")
	// removing "manage users" from a group must not leave the tenant without an administrator
	if p, err := perm.Normalize(in.Perms); err == nil && p[perm.Users] != perm.Write {
		for _, u := range s.a.Store.ListUsersIn(id.Tenant) {
			if auth.EffectiveGroup(u) == gid {
				remaining := 0
				for _, o := range s.a.Store.ListUsersIn(id.Tenant) {
					if auth.EffectiveGroup(o) != gid && s.a.Store.PermsFor(o)[perm.Users] == perm.Write {
						remaining++
					}
				}
				if remaining == 0 {
					writeErr(w, http.StatusBadRequest, "at least one user must be able to manage users")
					return
				}
				break
			}
		}
	}
	g, err := s.a.Store.UpdateGroup(id.Tenant, gid, in.Name, in.Description, in.Perms)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("group updated", "by", id.User, "tenant", id.Tenant, "group", g.Name)
	writeJSON(w, g)
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if err := s.a.Store.DeleteGroup(id.Tenant, r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
