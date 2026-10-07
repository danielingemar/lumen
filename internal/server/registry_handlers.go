package server

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/danielingemar/lumen/internal/buildinfo"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/model"
	"github.com/danielingemar/lumen/internal/perm"
	"github.com/danielingemar/lumen/internal/registry"
	"github.com/danielingemar/lumen/internal/status"
)

// WithRegistry enables hosts, instances, status and the agents' remote configuration.
func (s *Server) WithRegistry(r *registry.Service) *Server { s.reg = r; return s }

type snapshot struct {
	at   time.Time
	rows []model.Latest
	sum  status.Summary
}

type snapCache struct {
	mu sync.Mutex
	m  map[string]snapshot
}

const snapTTL = 10 * time.Second

// snapshot computes (or reuses for a few seconds) the up/down picture of a tenant. Dashboards show several
// status panels at once; they should cost one database query, not one each.
func (s *Server) snapshot(ctx context.Context, tenant string) (snapshot, error) {
	s.snaps.mu.Lock()
	if sn, ok := s.snaps.m[tenant]; ok && time.Since(sn.at) < snapTTL {
		s.snaps.mu.Unlock()
		return sn, nil
	}
	s.snaps.mu.Unlock()
	now := time.Now()
	rows, err := s.store.Latest(ctx, tenant, status.Names, now.Add(-24*time.Hour))
	if err != nil {
		return snapshot{}, err
	}
	sn := snapshot{at: now, rows: rows, sum: status.ComputeWith(now, rows, s.reg.ListHosts(tenant), s.reg.ListInstances(tenant), s.reg.GoneInstances(tenant))}
	s.snaps.mu.Lock()
	if s.snaps.m == nil {
		s.snaps.m = map[string]snapshot{}
	}
	s.snaps.m[tenant] = sn
	s.snaps.mu.Unlock()
	return sn, nil
}

func (s *Server) forget(tenant string) {
	s.snaps.mu.Lock()
	delete(s.snaps.m, tenant)
	s.snaps.mu.Unlock()
}

func (s *Server) regRoutes(mux *http.ServeMux) {
	if s.reg == nil {
		return
	}
	mux.Handle("GET /api/v1/status", s.needAny([]string{perm.Hosts, perm.Metrics}, s.statusSummary))
	mux.Handle("GET /api/v1/hosts", s.need(perm.Hosts, false, s.listHosts))
	s.hostGroupRoutes(mux)
	mux.Handle("GET /api/v1/hosts/{host}", s.need(perm.Hosts, false, s.getHost))
	mux.Handle("POST /api/v1/hosts/{host}/update", s.need(perm.Hosts, true, s.updateAgent))
	mux.Handle("POST /api/v1/hosts-update-all", s.need(perm.Hosts, true, s.updateAllAgents))
	mux.Handle("PUT /api/v1/hosts/{host}", s.need(perm.Hosts, true, s.putHost))
	mux.Handle("DELETE /api/v1/hosts/{host}", s.need(perm.Hosts, true, s.deleteHost))
	mux.Handle("POST /api/v1/hosts/{host}/remove", s.need(perm.Hosts, true, s.removeHost))
	mux.Handle("GET /api/v1/instances", s.need(perm.Hosts, false, s.listInstances))
	mux.Handle("POST /api/v1/instances", s.need(perm.Hosts, true, s.createInstance))
	mux.Handle("PUT /api/v1/instances/{id}", s.need(perm.Hosts, true, s.updateInstance))
	mux.Handle("DELETE /api/v1/instances/{id}", s.need(perm.Hosts, true, s.deleteInstance))
	mux.Handle("POST /api/v1/instance-keys/{key}/remove", s.need(perm.Hosts, true, s.removeUnmanaged))
	// agents (API keys only, never a browser session: the response contains instance credentials)
	mux.Handle("GET /api/v1/agent/config", s.authed("ingest", s.agentConfig))
}

func (s *Server) regErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, registry.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	default:
		s.log.Error("registry error", "err", err)
		writeErr(w, http.StatusServiceUnavailable, "the settings store is unavailable (the reason is in the server log: docker compose logs lumen, look for \"registry error\")")
	}
}

func (s *Server) statusSummary(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	sn, err := s.snapshot(r.Context(), id.Tenant)
	if err != nil {
		s.log.Error("status query failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "query failed")
		return
	}
	out := struct {
		status.Summary
		Alerts *alertCounts `json:"alerts,omitempty"`
	}{Summary: sn.sum}
	if s.alerts != nil && id.Can(perm.Alerts, false) {
		f, p := s.alerts.Counts(id.Tenant)
		out.Alerts = &alertCounts{f, p}
	}
	writeJSON(w, out)
}

type hostOut struct {
	status.Host
	Config          registry.HostConfig `json:"config"`
	UpdateRequested int64               `json:"update_requested,omitempty"` // when someone asked for an update of this agent (unix seconds), while it is open
}

func (s *Server) listHosts(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	sn, err := s.snapshot(r.Context(), id.Tenant)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed")
		return
	}
	cfgs := s.reg.ListHosts(id.Tenant)
	reqs := s.reg.UpdateRequests(id.Tenant)
	out := []hostOut{}
	for _, h := range sn.sum.HostList {
		c, ok := cfgs[h.Name]
		if !ok {
			c = registry.DefaultHost(h.Name)
		}
		out = append(out, hostOut{h, c, reqs[h.Name].Unix() * b2i(!reqs[h.Name].IsZero())})
	}
	writeJSON(w, map[string]any{"data": out})
}

func (s *Server) getHost(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	name := r.PathValue("host")
	sn, err := s.snapshot(r.Context(), id.Tenant)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed")
		return
	}
	h := status.Host{Name: name, Status: "pending"}
	for _, x := range allAgents(sn.sum) {
		if x.Name == name {
			h = x
		}
	}
	svcs, ctrs := status.Items(time.Now(), sn.rows, name)
	if svcs == nil {
		svcs = []status.Item{}
	}
	if ctrs == nil {
		ctrs = []status.Item{}
	}
	var asked int64
	if at, ok := s.reg.UpdateRequested(id.Tenant, name); ok {
		asked = at.Unix()
	}
	writeJSON(w, map[string]any{"host": hostOut{h, s.reg.GetHost(id.Tenant, name), asked}, "services": svcs, "containers": ctrs})
}

func (s *Server) putHost(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var body struct {
		registry.HostConfig
		GroupsRaw *json.RawMessage `json:"groups"` // outer field: wins over the embedded one, so that "absent" can be told from "empty"
	}
	if !readJSON(w, r, &body) {
		return
	}
	in := body.HostConfig
	in.Host = r.PathValue("host")
	if body.GroupsRaw == nil { // a client that does not know about groups (an old script, an older page) keeps what is there
		in.Groups = s.reg.GetHost(id.Tenant, in.Host).Groups
	} else if err := json.Unmarshal(*body.GroupsRaw, &in.Groups); err != nil {
		writeErr(w, http.StatusBadRequest, "groups must be a list of names")
		return
	}
	c, err := s.reg.PutHost(id.Tenant, in)
	if err != nil {
		s.regErr(w, err)
		return
	}
	s.forget(id.Tenant)
	s.log.Info("host settings changed", "by", id.User, "tenant", id.Tenant, "host", c.Host)
	writeJSON(w, c)
}

// removeHost takes a host out of the lists (it comes back if its agent reports again).
func (s *Server) removeHost(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if err := s.reg.RemoveHost(id.Tenant, r.PathValue("host")); err != nil {
		s.regErr(w, err)
		return
	}
	s.forget(id.Tenant)
	s.log.Info("host removed", "by", id.User, "tenant", id.Tenant, "host", r.PathValue("host"))
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) deleteHost(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if err := s.reg.DeleteHost(id.Tenant, r.PathValue("host")); err != nil {
		s.regErr(w, err)
		return
	}
	s.forget(id.Tenant)
	writeJSON(w, map[string]bool{"ok": true})
}

type instanceIn struct {
	Name          string `json:"name"`
	URL           string `json:"url"`
	Host          string `json:"host"`
	Token         string `json:"token"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	LogPath       string `json:"log_path"`
	ClearToken    bool   `json:"clear_token"`
	ClearPassword bool   `json:"clear_password"`
}

func (i instanceIn) reg() registry.InstanceIn {
	return registry.InstanceIn{Name: i.Name, URL: i.URL, Host: i.Host, Token: i.Token, Username: i.Username, Password: i.Password, LogPath: i.LogPath, ClearToken: i.ClearToken, ClearPassword: i.ClearPassword}
}

func (s *Server) listInstances(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	sn, err := s.snapshot(r.Context(), id.Tenant)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed")
		return
	}
	hosts, names := []string{}, map[string]string{}
	for _, h := range allAgents(sn.sum) {
		hosts = append(hosts, h.Name)
		if h.DisplayName != "" {
			names[h.Name] = h.DisplayName
		}
	}
	list := sn.sum.Instances
	if list == nil {
		list = []status.Instance{} // an empty list is [], not null
	}
	checkers := make([]hostOut, 0, len(sn.sum.Checkers))
	cfgs, reqs := s.reg.ListHosts(id.Tenant), s.reg.UpdateRequests(id.Tenant)
	for _, h := range sn.sum.Checkers {
		c, ok := cfgs[h.Name]
		if !ok {
			c = registry.DefaultHost(h.Name)
		}
		checkers = append(checkers, hostOut{h, c, reqs[h.Name].Unix() * b2i(!reqs[h.Name].IsZero())})
	}
	writeJSON(w, map[string]any{"data": list, "hosts": hosts, "host_names": names, "checkers": checkers})
}

func (s *Server) createInstance(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if !s.quotaOK(w, id, "instances") {
		return
	}
	var in instanceIn
	if !readJSON(w, r, &in) {
		return
	}
	i, err := s.reg.CreateInstance(id.Tenant, in.reg())
	if err != nil {
		s.regErr(w, err)
		return
	}
	s.forget(id.Tenant)
	s.log.Info("instance added", "by", id.User, "tenant", id.Tenant, "name", i.Name)
	writeJSON(w, i.Out())
}

func (s *Server) updateInstance(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in instanceIn
	if !readJSON(w, r, &in) {
		return
	}
	i, err := s.reg.UpdateInstance(id.Tenant, r.PathValue("id"), in.reg())
	if err != nil {
		s.regErr(w, err)
		return
	}
	s.forget(id.Tenant)
	s.log.Info("instance changed", "by", id.User, "tenant", id.Tenant, "name", i.Name)
	writeJSON(w, i.Out())
}

func (s *Server) deleteInstance(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if err := s.reg.DeleteInstance(id.Tenant, r.PathValue("id")); err != nil {
		s.regErr(w, err)
		return
	}
	s.forget(id.Tenant)
	s.auditOp(id, id.Tenant, "instance.remove", r.PathValue("id"), "")
	writeJSON(w, map[string]bool{"ok": true})
}

// removeUnmanaged takes an instance out of the list that nobody registered here (it was set up with agent flags on a
// machine, or it is only the leftover of one that was removed) and that has stopped answering. It comes back only if its
// agent reports it again.
func (s *Server) removeUnmanaged(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	key := r.PathValue("key")
	for _, i := range s.reg.ListInstances(id.Tenant) {
		if i.Out().Key == key {
			writeErr(w, http.StatusBadRequest, "this instance is managed here ("+i.Name+"): remove it with its own Remove button")
			return
		}
	}
	sn, err := s.snapshot(r.Context(), id.Tenant)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "cannot check the status right now: "+err.Error())
		return
	}
	var found *status.Instance
	for i := range sn.sum.Instances {
		if sn.sum.Instances[i].Key == key {
			found = &sn.sum.Instances[i]
		}
	}
	switch {
	case found == nil:
		writeErr(w, http.StatusNotFound, "no such instance in the list (it may already be removed)")
		return
	case found.Status == "up":
		writeErr(w, http.StatusBadRequest, "this instance is up and its agent still reports it. Stop checking it where it is set up (the agent's settings on the machine) and it can be removed from the list")
		return
	case time.Since(time.Unix(found.LastSeen, 0)) <= status.Fresh:
		writeErr(w, http.StatusBadRequest, "its agent still reports it (it cannot reach it). Take it out of the agent's settings on the machine, or stop that agent: then it is gone from the list on its own, or you can remove it here once it has stopped reporting. Removed now it would only come back")
		return
	}
	// dated so that anything reported after this moment counts as the instance being back
	if err := s.reg.MarkGoneAt(id.Tenant, key, found.Name, time.Now().Add(-status.RemovedGrace)); err != nil {
		s.regErr(w, err)
		return
	}
	s.forget(id.Tenant)
	s.auditOp(id, id.Tenant, "instance.remove", key, found.Name)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) agentConfig(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	host := r.URL.Query().Get("host")
	if host == "" {
		writeErr(w, http.StatusBadRequest, "host is required")
		return
	}
	cfg, err := s.reg.AgentConfig(id.Tenant, host)
	if err != nil {
		s.regErr(w, err)
		return
	}
	// someone asked in the UI for this agent to be updated: tell it which version it should have, until it has it
	if _, asked := s.reg.UpdateRequested(id.Tenant, host); asked {
		switch v := r.URL.Query().Get("version"); {
		case buildinfo.Version == "" || buildinfo.Version == "dev":
		case v == buildinfo.Version:
			s.reg.ClearUpdate(id.Tenant, host)
		default:
			cfg.UpdateTo = buildinfo.Version
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, cfg)
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// whyNotUpdatable explains why the one-click update cannot be used for a host, or returns "" if it can.
func whyNotUpdatable(h status.Host) string {
	switch {
	case buildinfo.Version == "" || buildinfo.Version == "dev":
		return "this server was built without a version stamp (a plain go build), so it does not know which version the agents should have"
	case h.Version == "" || h.Version == buildinfo.Version:
		return "this agent already has the version of this server"
	case h.SelfUpdate == "":
		return "this agent cannot update itself: it was installed before one-click updates, or it is a Windows agent. Run the installer again on the machine (on Linux: add --update to keep its settings); for an agent in a container, restart the container"
	}
	return ""
}

// updateAgent asks for the agent on one host to be updated to this server's version.
func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	name := r.PathValue("host")
	sn, err := s.snapshot(r.Context(), id.Tenant)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed")
		return
	}
	for _, h := range allAgents(sn.sum) {
		if h.Name != name {
			continue
		}
		if why := whyNotUpdatable(h); why != "" {
			writeErr(w, http.StatusBadRequest, why)
			return
		}
		if err := s.reg.RequestUpdate(id.Tenant, name); err != nil {
			s.regErr(w, err)
			return
		}
		s.auditOp(id, id.Tenant, "agent.update", name, h.Version+" to "+buildinfo.Version)
		writeJSON(w, map[string]any{"ok": true, "mode": h.SelfUpdate, "target": buildinfo.Version})
		return
	}
	writeErr(w, http.StatusNotFound, "no such host")
}

// updateAllAgents asks every agent that can update itself and has another version than this server to do so.
func (s *Server) updateAllAgents(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	sn, err := s.snapshot(r.Context(), id.Tenant)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed")
		return
	}
	if buildinfo.Version == "" || buildinfo.Version == "dev" {
		writeErr(w, http.StatusBadRequest, whyNotUpdatable(status.Host{}))
		return
	}
	requested, manual := []string{}, []string{}
	for _, h := range allAgents(sn.sum) {
		switch why := whyNotUpdatable(h); {
		case why == "":
			if s.reg.RequestUpdate(id.Tenant, h.Name) == nil {
				requested = append(requested, h.Name)
			}
		case h.Version != "" && h.Version != buildinfo.Version: // outdated but cannot update itself
			manual = append(manual, h.Name)
		}
	}
	if len(requested) > 0 {
		s.auditOp(id, id.Tenant, "agent.update", strings.Join(requested, ","), "to "+buildinfo.Version)
	}
	writeJSON(w, map[string]any{"requested": requested, "manual": manual, "target": buildinfo.Version})
}

// allAgents are the machines and the agents that only check instances: both are agents that can be updated and that
// instances can be assigned to, but only the machines are hosts.
func allAgents(sum status.Summary) []status.Host {
	out := make([]status.Host, 0, len(sum.HostList)+len(sum.Checkers))
	out = append(out, sum.HostList...)
	return append(out, sum.Checkers...)
}
