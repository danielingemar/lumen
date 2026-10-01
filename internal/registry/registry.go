// Package registry stores what an administrator configures in the UI: per-host agent settings and the Nextcloud
// instances to monitor. Agents fetch their part of it from the server, so nothing has to be edited on the machines.
package registry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/secretbox"
)

const (
	collHosts = "hosts"
	collInst  = "instances"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
	hostRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	nameRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,59}$`)
)

func invalid(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(f, a...)) }

// HostConfig is what an agent on one machine should do. A host without a stored config gets the defaults.
type HostConfig struct {
	Tenant        string    `json:"tenant,omitempty"`
	Host          string    `json:"host"`
	LogPaths      []string  `json:"log_paths"`      // files or globs to ship, e.g. /var/log/nginx/*.log
	DockerLogs    bool      `json:"docker_logs"`    // ship the logs of all Docker containers
	Systemd       bool      `json:"systemd"`        // report systemd services
	Containers    bool      `json:"containers"`     // report Docker containers
	WatchServices []string  `json:"watch_services"` // services that must run: reported as down even when stopped
	Updated       time.Time `json:"updated,omitempty"`
}

func DefaultHost(host string) HostConfig {
	return HostConfig{Host: host, LogPaths: []string{}, WatchServices: []string{}, Systemd: true, Containers: true}
}

// Instance is one Nextcloud installation, checked by the agent on Host.
type Instance struct {
	ID          string    `json:"id"`
	Tenant      string    `json:"tenant,omitempty"`
	Name        string    `json:"name"`
	URL         string    `json:"url"`
	Host        string    `json:"host"` // the machine whose agent runs the checks
	TokenEnc    string    `json:"token_enc,omitempty"`
	Username    string    `json:"username,omitempty"`
	PasswordEnc string    `json:"password_enc,omitempty"`
	LogPath     string    `json:"log_path,omitempty"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
}

// InstanceOut is an instance as shown in the UI: secrets are never sent back, only whether they are set.
type InstanceOut struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Host        string `json:"host"`
	Username    string `json:"username"`
	LogPath     string `json:"log_path"`
	HasToken    bool   `json:"has_token"`
	HasPassword bool   `json:"has_password"`
	Key         string `json:"key"` // the "instance" label its metrics carry
	Created     string `json:"created"`
}

func (i Instance) Out() InstanceOut {
	return InstanceOut{ID: i.ID, Name: i.Name, URL: i.URL, Host: i.Host, Username: i.Username, LogPath: i.LogPath,
		HasToken: i.TokenEnc != "", HasPassword: i.PasswordEnc != "", Key: MetricKey(i.URL), Created: i.Created.Format(time.RFC3339)}
}

// MetricKey is the value of the "instance" label the agent puts on a Nextcloud instance's metrics (the URL's host[:port]).
func MetricKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// InstanceIn is a create/update request. Empty secrets keep the stored ones; Clear* removes them.
type InstanceIn struct {
	Name, URL, Host, Username, LogPath string
	Token, Password                    string
	ClearToken, ClearPassword          bool
}

// AgentInstance is an instance as the agent receives it (secrets decrypted).
type AgentInstance struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Token    string `json:"token,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	LogPath  string `json:"log_path,omitempty"`
}

// AgentConfig is the complete remote configuration for one host.
type AgentConfig struct {
	Revision      string          `json:"revision"`
	LogPaths      []string        `json:"log_paths"`
	DockerLogs    bool            `json:"docker_logs"`
	Systemd       bool            `json:"systemd"`
	Containers    bool            `json:"containers"`
	WatchServices []string        `json:"watch_services"`
	Instances     []AgentInstance `json:"instances"`
}

type Service struct {
	b   docstore.Backend
	box *secretbox.Box
}

func New(b docstore.Backend, box *secretbox.Box) *Service { return &Service{b, box} }

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func hostID(tenant, host string) string { return tenant + ":" + host }

// ---- hosts ----

func cleanList(in []string, max, maxLen int, what string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		if len(s) > maxLen || strings.ContainsAny(s, "\r\n\x00") {
			return nil, invalid("%s entry %.30q is too long or has control characters", what, s)
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) > max {
		return nil, invalid("at most %d %s entries", max, what)
	}
	return out, nil
}

func (s *Service) GetHost(tenant, host string) HostConfig {
	c, cancel := ctx()
	defer cancel()
	d, err := s.b.Get(c, collHosts, hostID(tenant, host))
	var h HostConfig
	if err != nil || d.Decode(&h) != nil || h.Tenant != tenant {
		return DefaultHost(host)
	}
	if h.LogPaths == nil {
		h.LogPaths = []string{}
	}
	if h.WatchServices == nil {
		h.WatchServices = []string{}
	}
	return h
}

func (s *Service) ListHosts(tenant string) map[string]HostConfig {
	c, cancel := ctx()
	defer cancel()
	docs, _ := s.b.List(c, collHosts, map[string]string{"tenant": tenant}, 1000)
	out := map[string]HostConfig{}
	for _, d := range docs {
		var h HostConfig
		if d.Decode(&h) == nil {
			out[h.Host] = h
		}
	}
	return out
}

func (s *Service) PutHost(tenant string, in HostConfig) (HostConfig, error) {
	if !hostRe.MatchString(in.Host) {
		return HostConfig{}, invalid("host name must be letters, digits, . _ - (max 128)")
	}
	var err error
	if in.LogPaths, err = cleanList(in.LogPaths, 30, 300, "log path"); err != nil {
		return HostConfig{}, err
	}
	if in.WatchServices, err = cleanList(in.WatchServices, 100, 100, "service"); err != nil {
		return HostConfig{}, err
	}
	in.Tenant, in.Updated = tenant, time.Now().UTC()
	c, cancel := ctx()
	defer cancel()
	return in, s.b.Put(c, collHosts, hostID(tenant, in.Host), in, "")
}

func (s *Service) DeleteHost(tenant, host string) error {
	c, cancel := ctx()
	defer cancel()
	return s.b.Delete(c, collHosts, hostID(tenant, host))
}

// ---- instances ----

func (s *Service) ListInstances(tenant string) []Instance {
	c, cancel := ctx()
	defer cancel()
	docs, _ := s.b.List(c, collInst, map[string]string{"tenant": tenant}, 1000)
	out := []Instance{}
	for _, d := range docs {
		var i Instance
		if d.Decode(&i) == nil {
			out = append(out, i)
		}
	}
	sort.Slice(out, func(a, b int) bool { return strings.ToLower(out[a].Name) < strings.ToLower(out[b].Name) })
	return out
}

func (s *Service) getInstance(tenant, id string) (Instance, string, error) {
	c, cancel := ctx()
	defer cancel()
	d, err := s.b.Get(c, collInst, id)
	var i Instance
	if err != nil || d.Decode(&i) != nil || i.Tenant != tenant {
		return Instance{}, "", ErrNotFound
	}
	return i, d.Version, nil
}

func (s *Service) validateInstance(in *InstanceIn) error {
	in.Name, in.URL, in.Host = strings.TrimSpace(in.Name), strings.TrimSpace(in.URL), strings.TrimSpace(in.Host)
	in.LogPath, in.Username = strings.TrimSpace(in.LogPath), strings.TrimSpace(in.Username)
	if !nameRe.MatchString(in.Name) {
		return invalid("the name must be letters, digits, . _ - (no spaces, max 60)")
	}
	u, err := url.Parse(in.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return invalid("the URL must look like https://cloud.example.com (no user:password in it)")
	}
	in.URL = strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/")
	if !hostRe.MatchString(in.Host) {
		return invalid("choose which machine's agent runs the checks")
	}
	if len(in.LogPath) > 300 || strings.ContainsAny(in.LogPath+in.Username, "\r\n\x00") || len(in.Token) > 300 || len(in.Password) > 300 || len(in.Username) > 100 {
		return invalid("a field is too long or has control characters")
	}
	return nil
}

func (s *Service) uniqueName(tenant, name, exceptID string) error {
	for _, i := range s.ListInstances(tenant) {
		if i.ID != exceptID && strings.EqualFold(i.Name, name) {
			return invalid("an instance with that name already exists")
		}
	}
	return nil
}

func (s *Service) CreateInstance(tenant string, in InstanceIn) (Instance, error) {
	if err := s.validateInstance(&in); err != nil {
		return Instance{}, err
	}
	if err := s.uniqueName(tenant, in.Name, ""); err != nil {
		return Instance{}, err
	}
	now := time.Now().UTC()
	i := Instance{ID: "i" + randID(), Tenant: tenant, Name: in.Name, URL: in.URL, Host: in.Host, Username: in.Username, LogPath: in.LogPath,
		TokenEnc: s.box.Seal(in.Token), PasswordEnc: s.box.Seal(in.Password), Created: now, Updated: now}
	c, cancel := ctx()
	defer cancel()
	return i, s.b.Create(c, collInst, i.ID, i)
}

func (s *Service) UpdateInstance(tenant, id string, in InstanceIn) (Instance, error) {
	cur, ver, err := s.getInstance(tenant, id)
	if err != nil {
		return Instance{}, err
	}
	if err := s.validateInstance(&in); err != nil {
		return Instance{}, err
	}
	if err := s.uniqueName(tenant, in.Name, id); err != nil {
		return Instance{}, err
	}
	cur.Name, cur.URL, cur.Host, cur.Username, cur.LogPath = in.Name, in.URL, in.Host, in.Username, in.LogPath
	switch {
	case in.ClearToken:
		cur.TokenEnc = ""
	case in.Token != "":
		cur.TokenEnc = s.box.Seal(in.Token)
	}
	switch {
	case in.ClearPassword:
		cur.PasswordEnc = ""
	case in.Password != "":
		cur.PasswordEnc = s.box.Seal(in.Password)
	}
	cur.Updated = time.Now().UTC()
	c, cancel := ctx()
	defer cancel()
	return cur, s.b.Put(c, collInst, id, cur, ver)
}

func (s *Service) DeleteInstance(tenant, id string) error {
	if _, _, err := s.getInstance(tenant, id); err != nil {
		return err
	}
	c, cancel := ctx()
	defer cancel()
	return s.b.Delete(c, collInst, id)
}

// AgentConfig builds the remote configuration for one host, with decrypted secrets, and a revision hash so the
// agent only restarts its collectors when something actually changed.
func (s *Service) AgentConfig(tenant, host string) (AgentConfig, error) {
	h := s.GetHost(tenant, host)
	cfg := AgentConfig{LogPaths: h.LogPaths, DockerLogs: h.DockerLogs, Systemd: h.Systemd, Containers: h.Containers, WatchServices: h.WatchServices, Instances: []AgentInstance{}}
	for _, i := range s.ListInstances(tenant) {
		if i.Host != host {
			continue
		}
		tok, err1 := s.box.Open(i.TokenEnc)
		pw, err2 := s.box.Open(i.PasswordEnc)
		if err1 != nil || err2 != nil { // key changed: monitor without credentials rather than not at all
			tok, pw = "", ""
		}
		cfg.Instances = append(cfg.Instances, AgentInstance{ID: i.ID, Name: i.Name, URL: i.URL, Token: tok, Username: i.Username, Password: pw, LogPath: i.LogPath})
	}
	b, _ := json.Marshal(cfg)
	sum := sha256.Sum256(b)
	cfg.Revision = hex.EncodeToString(sum[:8])
	return cfg, nil
}

func randID() string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		panic("no randomness available")
	}
	return hex.EncodeToString(b)
}
