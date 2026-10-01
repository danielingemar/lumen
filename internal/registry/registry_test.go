package registry

import (
	"context"
	"strings"
	"testing"

	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/secretbox"
)

func newReg(t *testing.T) (*Service, *docstore.File) {
	f, _ := docstore.OpenFile(t.TempDir())
	box, _ := secretbox.New("test-secret")
	return New(f, box), f
}

func TestSecretBox(t *testing.T) {
	a, _ := secretbox.New("k1")
	b, _ := secretbox.New("k2")
	s := a.Seal("tok-123")
	if s == "tok-123" || strings.Contains(s, "tok") {
		t.Fatal("must not be plain text")
	}
	if p, err := a.Open(s); err != nil || p != "tok-123" {
		t.Fatal(p, err)
	}
	if _, err := b.Open(s); err == nil {
		t.Fatal("a different key must not decrypt")
	}
	if a.Seal("x") == a.Seal("x") {
		t.Fatal("nonce must differ")
	}
	if a.Seal("") != "" {
		t.Fatal("empty stays empty")
	}
	if _, err := a.Open(s[:len(s)-3] + "AAA"); err == nil {
		t.Fatal("tampering must be detected")
	}
}

func TestInstanceLifecycleAndSecrets(t *testing.T) {
	s, f := newReg(t)
	i, err := s.CreateInstance("acme", InstanceIn{Name: "ks", URL: "https://cloud.example.com/", Host: "web1", Token: "SECRET-TOKEN", LogPath: "/var/www/nextcloud/data/nextcloud.log"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := f.Get(context.Background(), "instances", i.ID)
	if strings.Contains(string(raw.Data), "SECRET-TOKEN") {
		t.Fatal("the token must be encrypted at rest")
	}
	o := i.Out()
	if !o.HasToken || o.HasPassword || o.Key != "cloud.example.com" || o.URL != "https://cloud.example.com" {
		t.Fatalf("out: %+v", o)
	}
	cfg, _ := s.AgentConfig("acme", "web1")
	if len(cfg.Instances) != 1 || cfg.Instances[0].Token != "SECRET-TOKEN" || cfg.Instances[0].LogPath == "" {
		t.Fatalf("the agent receives the decrypted token: %+v", cfg)
	}
	if other, _ := s.AgentConfig("acme", "web2"); len(other.Instances) != 0 {
		t.Fatal("an agent only gets the instances assigned to its host")
	}
	if other, _ := s.AgentConfig("globex", "web1"); len(other.Instances) != 0 {
		t.Fatal("tenants are isolated")
	}
	// fix a typo in the URL without retyping the token
	u, err := s.UpdateInstance("acme", i.ID, InstanceIn{Name: "ks", URL: "https://cloud.example.org", Host: "web1"})
	if err != nil || !u.Out().HasToken {
		t.Fatalf("an update with an empty token must keep it: %+v %v", u.Out(), err)
	}
	cfg2, _ := s.AgentConfig("acme", "web1")
	if cfg2.Instances[0].URL != "https://cloud.example.org" || cfg2.Instances[0].Token != "SECRET-TOKEN" || cfg2.Revision == cfg.Revision {
		t.Fatal("the new URL must reach the agent with a new revision, token intact")
	}
	u, _ = s.UpdateInstance("acme", i.ID, InstanceIn{Name: "ks", URL: "https://cloud.example.org", Host: "web1", Token: "NEW"})
	cfg3, _ := s.AgentConfig("acme", "web1")
	if cfg3.Instances[0].Token != "NEW" {
		t.Fatal("a new token replaces the old one")
	}
	u, _ = s.UpdateInstance("acme", i.ID, InstanceIn{Name: "ks", URL: "https://cloud.example.org", Host: "web1", ClearToken: true})
	if u.Out().HasToken {
		t.Fatal("clear_token removes it")
	}
	// moving the instance to another machine
	s.UpdateInstance("acme", i.ID, InstanceIn{Name: "ks", URL: "https://cloud.example.org", Host: "web2"})
	if c, _ := s.AgentConfig("acme", "web1"); len(c.Instances) != 0 {
		t.Fatal("moved away from web1")
	}
	if _, err := s.UpdateInstance("globex", i.ID, InstanceIn{Name: "x", URL: "https://x.example.com", Host: "web1"}); err == nil {
		t.Fatal("another tenant must not edit it")
	}
	if err := s.DeleteInstance("globex", i.ID); err == nil {
		t.Fatal("another tenant must not delete it")
	}
	if err := s.DeleteInstance("acme", i.ID); err != nil || len(s.ListInstances("acme")) != 0 {
		t.Fatal("delete")
	}
}

func TestInstanceValidation(t *testing.T) {
	s, _ := newReg(t)
	ok := InstanceIn{Name: "a", URL: "https://a.example.com", Host: "h"}
	if _, err := s.CreateInstance("acme", ok); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInstance("acme", InstanceIn{Name: "A", URL: "https://b.example.com", Host: "h"}); err == nil {
		t.Fatal("names are unique per tenant, case-insensitively")
	}
	for _, bad := range []InstanceIn{
		{Name: "", URL: "https://x.com", Host: "h"}, {Name: "has space", URL: "https://x.com", Host: "h"},
		{Name: "n1", URL: "ftp://x.com", Host: "h"}, {Name: "n2", URL: "cloud.example.com", Host: "h"},
		{Name: "n3", URL: "https://user:pw@x.com", Host: "h"}, {Name: "n4", URL: "https://x.com", Host: ""},
		{Name: "n5", URL: "https://x.com", Host: "bad host!"}, {Name: "n6", URL: "https://x.com", Host: "h", LogPath: "a\nb"},
	} {
		if _, err := s.CreateInstance("acme", bad); err == nil {
			t.Errorf("%+v must be rejected", bad)
		}
	}
}

func TestHostConfig(t *testing.T) {
	s, _ := newReg(t)
	d := s.GetHost("acme", "web1")
	if !d.Systemd || !d.Containers || d.DockerLogs || len(d.LogPaths) != 0 {
		t.Fatalf("defaults: %+v", d)
	}
	h, err := s.PutHost("acme", HostConfig{Host: "web1", LogPaths: []string{" /var/log/nginx/*.log ", "/var/log/nginx/*.log", ""}, DockerLogs: true, Systemd: true, WatchServices: []string{"nginx", "postfix"}})
	if err != nil || len(h.LogPaths) != 1 || h.LogPaths[0] != "/var/log/nginx/*.log" {
		t.Fatalf("paths are trimmed and de-duplicated: %+v %v", h, err)
	}
	if g := s.GetHost("acme", "web1"); !g.DockerLogs || g.Containers || len(g.WatchServices) != 2 {
		t.Fatalf("saved values must round-trip exactly (containers was switched off): %+v", g)
	}
	if g := s.GetHost("globex", "web1"); g.DockerLogs {
		t.Fatal("tenants are isolated")
	}
	c1, _ := s.AgentConfig("acme", "web1")
	s.PutHost("acme", HostConfig{Host: "web1", LogPaths: []string{"/a"}, Systemd: true, Containers: true})
	c2, _ := s.AgentConfig("acme", "web1")
	if c1.Revision == c2.Revision {
		t.Fatal("the revision must change when the config changes")
	}
	c3, _ := s.AgentConfig("acme", "web1")
	if c2.Revision != c3.Revision {
		t.Fatal("the revision must be stable when nothing changed")
	}
	for _, bad := range []HostConfig{{Host: "bad host"}, {Host: ""}, {Host: "h", LogPaths: []string{"a\nb"}}, {Host: "h", LogPaths: make([]string, 31)}} {
		if len(bad.LogPaths) == 31 { // too many entries: make them distinct so de-duplication does not hide the problem
			for i := range bad.LogPaths {
				bad.LogPaths[i] = "/x" + string(rune('a'+i%26)) + string(rune('a'+i/26))
			}
		}
		if _, err := s.PutHost("acme", bad); err == nil {
			t.Errorf("%+v must be rejected", bad)
		}
	}
}
