package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/perm"
	"github.com/danielingemar/lumen/internal/registry"
)

// noHost is what a query is limited to when the group asked for has no hosts: a name that no host can have (it is not a valid
// host name), so that the answer is "nothing" rather than "everything".
const noHost = "?no host in this group"

// groupHosts turns the name of a host group into the hosts to limit a query to. No group asked for: no limit (nil).
func (s *Server) groupHosts(tenant, group string) []string {
	group = strings.TrimSpace(group)
	if group == "" || s.reg == nil {
		return nil
	}
	if h := s.reg.GroupHosts(tenant, group); len(h) > 0 {
		return h
	}
	return []string{noHost}
}

func (s *Server) hostGroupRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/host-groups", s.need(perm.Hosts, false, s.listHostGroups))
	mux.Handle("POST /api/v1/host-groups/members", s.need(perm.Hosts, true, s.changeGroupMembers))
	mux.Handle("POST /api/v1/host-groups/rename", s.need(perm.Hosts, true, s.renameHostGroup))
	mux.Handle("POST /api/v1/host-groups/delete", s.need(perm.Hosts, true, s.deleteHostGroup))
}

func (s *Server) listHostGroups(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	writeJSON(w, map[string]any{"data": s.reg.Groups(id.Tenant)})
}

// changeGroupMembers adds hosts to a group and removes others from it, in one go (many hosts at a time).
func (s *Server) changeGroupMembers(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in struct {
		Group  string   `json:"group"`
		Add    []string `json:"add"`
		Remove []string `json:"remove"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	n, err := s.reg.ChangeMembers(id.Tenant, in.Group, in.Add, in.Remove)
	if err != nil {
		s.regErr(w, err)
		return
	}
	s.forget(id.Tenant)
	s.auditOp(id, id.Tenant, "hostgroup.members", in.Group, strings.Join(append(append([]string{}, prefixed("+", in.Add)...), prefixed("-", in.Remove)...), " "))
	writeJSON(w, map[string]any{"ok": true, "changed": n})
}

func prefixed(p string, xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = p + x
	}
	return out
}

func (s *Server) renameHostGroup(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	n, err := s.reg.RenameGroup(id.Tenant, in.From, in.To)
	if err != nil {
		s.regErr(w, err)
		return
	}
	s.forget(id.Tenant)
	rules := 0
	if s.alerts != nil && n > 0 { // rules limited to the group follow it
		to, _ := registry.NormalizeGroup(in.To)
		rules = s.alerts.RenameRuleGroup(id.Tenant, in.From, to)
	}
	s.auditOp(id, id.Tenant, "hostgroup.rename", in.From, "to "+in.To)
	writeJSON(w, map[string]any{"ok": true, "changed": n, "rules": rules})
}

func (s *Server) deleteHostGroup(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in struct {
		Group string `json:"group"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	n, err := s.reg.DeleteGroup(id.Tenant, in.Group)
	if err != nil {
		s.regErr(w, err)
		return
	}
	s.forget(id.Tenant)
	s.auditOp(id, id.Tenant, "hostgroup.delete", in.Group, "")
	writeJSON(w, map[string]any{"ok": true, "changed": n})
}

var _ = json.Marshal
