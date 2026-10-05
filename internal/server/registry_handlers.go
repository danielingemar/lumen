package server

import (
	"context"
	"errors"
	"net/http"
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
	sn := snapshot{at: now, rows: rows, sum: status.Compute(now, rows, s.reg.ListHosts(tenant), s.reg.ListInstances(tenant))}
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
	mux.Handle("GET /api/v1/hosts/{host}", s.need(perm.Hosts, false, s.getHost))
	mux.Handle("PUT /api/v1/hosts/{host}", s.need(perm.Hosts, true, s.putHost))
	mux.Handle("DELETE /api/v1/hosts/{host}", s.need(perm.Hosts, true, s.deleteHost))
	mux.Handle("POST /api/v1/hosts/{host}/remove", s.need(perm.Hosts, true, s.removeHost))
	mux.Handle("GET /api/v1/instances", s.need(perm.Hosts, false, s.listInstances))
	mux.Handle("POST /api/v1/instances", s.need(perm.Hosts, true, s.createInstance))
	mux.Handle("PUT /api/v1/instances/{id}", s.need(perm.Hosts, true, s.updateInstance))
	mux.Handle("DELETE /api/v1/instances/{id}", s.need(perm.Hosts, true, s.deleteInstance))
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
		writeErr(w, http.StatusServiceUnavailable, "the settings store is unavailable")
	}
}

func (s *Server) statusSummary(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	sn, err := s.snapshot(r.Context(), id.Tenant)
	if err != nil {
		s.log.Error("status query failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "query failed")
		return
	}
	writeJSON(w, sn.sum)
}

type hostOut struct {
	status.Host
	Config registry.HostConfig `json:"config"`
}

func (s *Server) listHosts(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	sn, err := s.snapshot(r.Context(), id.Tenant)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed")
		return
	}
	cfgs := s.reg.ListHosts(id.Tenant)
	out := []hostOut{}
	for _, h := range sn.sum.HostList {
		c, ok := cfgs[h.Name]
		if !ok {
			c = registry.DefaultHost(h.Name)
		}
		out = append(out, hostOut{h, c})
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
	for _, x := range sn.sum.HostList {
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
	writeJSON(w, map[string]any{"host": hostOut{h, s.reg.GetHost(id.Tenant, name)}, "services": svcs, "containers": ctrs})
}

func (s *Server) putHost(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in registry.HostConfig
	if !readJSON(w, r, &in) {
		return
	}
	in.Host = r.PathValue("host")
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
	for _, h := range sn.sum.HostList {
		hosts = append(hosts, h.Name)
		if h.DisplayName != "" {
			names[h.Name] = h.DisplayName
		}
	}
	writeJSON(w, map[string]any{"data": sn.sum.Instances, "hosts": hosts, "host_names": names})
}

func (s *Server) createInstance(w http.ResponseWriter, r *http.Request, id edition.Identity) {
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
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, cfg)
}
