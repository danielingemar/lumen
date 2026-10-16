package registry

import (
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Redis is what Lumen knows about one Redis server beyond what its agent finds by itself: the password (encrypted), an
// ACL user, and, for a Redis that is not in Docker, its address. A Redis in Docker is matched by Container on Host.
type Redis struct {
	ID          string    `json:"id"`
	Tenant      string    `json:"tenant,omitempty"`
	Name        string    `json:"name"`      // what it is called in Lumen
	Host        string    `json:"host"`      // the machine whose agent connects to it
	Container   string    `json:"container"` // Docker container name (empty for one given by address)
	Addr        string    `json:"addr"`      // host:port, optional for a container
	Username    string    `json:"username,omitempty"`
	PasswordEnc string    `json:"password_enc,omitempty"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
}

// RedisOut is a Redis as the UI sees it: the password is never sent back, only whether one is set.
type RedisOut struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Host        string `json:"host"`
	Container   string `json:"container"`
	Addr        string `json:"addr"`
	Username    string `json:"username"`
	HasPassword bool   `json:"has_password"`
}

func (r Redis) Out() RedisOut {
	return RedisOut{ID: r.ID, Name: r.Name, Host: r.Host, Container: r.Container, Addr: r.Addr, Username: r.Username, HasPassword: r.PasswordEnc != ""}
}

type RedisIn struct {
	Name, Host, Container, Addr, Username string
	Password                              string
	ClearPassword                         bool
}

// AgentRedis is a Redis as the agent receives it (password decrypted).
type AgentRedis struct {
	Name      string `json:"name"`
	Container string `json:"container,omitempty"`
	Addr      string `json:"addr,omitempty"`
	Username  string `json:"username,omitempty"`
	Password  string `json:"password,omitempty"`
}

func (s *Service) ListRedis(tenant string) []Redis {
	c, cancel := ctx()
	defer cancel()
	docs, _ := s.b.List(c, collRedis, map[string]string{"tenant": tenant}, 1000)
	out := []Redis{}
	for _, d := range docs {
		var r Redis
		if d.Decode(&r) == nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		return strings.ToLower(out[a].Host+"/"+out[a].Name) < strings.ToLower(out[b].Host+"/"+out[b].Name)
	})
	return out
}

func (s *Service) getRedis(tenant, id string) (Redis, string, error) {
	c, cancel := ctx()
	defer cancel()
	d, err := s.b.Get(c, collRedis, id)
	var r Redis
	if err != nil || d.Decode(&r) != nil || r.Tenant != tenant {
		return Redis{}, "", ErrNotFound
	}
	return r, d.Version, nil
}

func (s *Service) validateRedis(tenant, id string, in *RedisIn) error {
	in.Name, in.Host, in.Container = strings.TrimSpace(in.Name), strings.TrimSpace(in.Host), strings.TrimSpace(in.Container)
	in.Addr, in.Username = strings.TrimSpace(in.Addr), strings.TrimSpace(in.Username)
	if in.Name == "" {
		in.Name = in.Container
	}
	if in.Name == "" {
		in.Name = in.Addr
	}
	if !hostRe.MatchString(in.Host) {
		return invalid("choose which machine's agent connects to this Redis")
	}
	if in.Container == "" && in.Addr == "" {
		return invalid("give the Docker container name or an address (host:port)")
	}
	if in.Name == "" || len(in.Name) > 100 || strings.ContainsAny(in.Name+in.Container+in.Username, "\r\n\x00") {
		return invalid("the name is empty, too long or has control characters")
	}
	if in.Container != "" && !nameRe.MatchString(in.Container) {
		return invalid("the container name must be letters, digits, . _ - (as Docker allows)")
	}
	if in.Addr != "" {
		h, p, err := net.SplitHostPort(in.Addr)
		n, _ := strconv.Atoi(p)
		if err != nil || h == "" || n < 1 || n > 65535 || strings.ContainsAny(in.Addr, " \r\n\x00/") {
			return invalid("the address must look like 10.0.0.5:6379 or redis.internal:6379")
		}
	}
	if len(in.Password) > 500 || len(in.Username) > 100 || strings.ContainsAny(in.Password, "\r\n\x00") {
		return invalid("the password or user is too long or has control characters")
	}
	for _, o := range s.ListRedis(tenant) {
		if o.ID != id && o.Host == in.Host && (strings.EqualFold(o.Name, in.Name) || (in.Container != "" && o.Container == in.Container)) {
			return invalid("this Redis is already set up on that machine")
		}
	}
	return nil
}

func (s *Service) CreateRedis(tenant string, in RedisIn) (Redis, error) {
	if err := s.validateRedis(tenant, "", &in); err != nil {
		return Redis{}, err
	}
	now := time.Now().UTC()
	r := Redis{ID: "r" + randID(), Tenant: tenant, Name: in.Name, Host: in.Host, Container: in.Container, Addr: in.Addr, Username: in.Username,
		PasswordEnc: s.box.Seal(in.Password), Created: now, Updated: now}
	c, cancel := ctx()
	defer cancel()
	return r, s.b.Create(c, collRedis, r.ID, r)
}

func (s *Service) UpdateRedis(tenant, id string, in RedisIn) (Redis, error) {
	cur, ver, err := s.getRedis(tenant, id)
	if err != nil {
		return Redis{}, err
	}
	if err := s.validateRedis(tenant, id, &in); err != nil {
		return Redis{}, err
	}
	cur.Name, cur.Host, cur.Container, cur.Addr, cur.Username = in.Name, in.Host, in.Container, in.Addr, in.Username
	switch {
	case in.ClearPassword:
		cur.PasswordEnc = ""
	case in.Password != "":
		cur.PasswordEnc = s.box.Seal(in.Password)
	}
	cur.Updated = time.Now().UTC()
	c, cancel := ctx()
	defer cancel()
	return cur, s.b.Put(c, collRedis, id, cur, ver)
}

func (s *Service) DeleteRedis(tenant, id string) error {
	if _, _, err := s.getRedis(tenant, id); err != nil {
		return err
	}
	c, cancel := ctx()
	defer cancel()
	return s.b.Delete(c, collRedis, id)
}
