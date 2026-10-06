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
		{Name: "n7", URL: "https://ks.example.com/login", Host: "h"}, {Name: "n8", URL: "https://ks.example.com/index.php/apps/files", Host: "h"},
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

func TestSubdirectoryInstallIsAllowed(t *testing.T) {
	s, _ := newReg(t)
	i, err := s.CreateInstance("acme", InstanceIn{Name: "sub", URL: "https://example.com/nextcloud/", Host: "h"})
	if err != nil || i.URL != "https://example.com/nextcloud" {
		t.Fatalf("Nextcloud installed in a subdirectory must stay possible: %+v %v", i, err)
	}
}

func TestHostDisplayNameAndRemoval(t *testing.T) {
	s, _ := newReg(t)
	h, err := s.PutHost("acme", HostConfig{Host: "b4d226bb6dfe", DisplayName: "  Nextcloud KS  ", Systemd: true})
	if err != nil || h.DisplayName != "Nextcloud KS" || s.GetHost("acme", "b4d226bb6dfe").DisplayName != "Nextcloud KS" {
		t.Fatalf("rename: %+v %v", h, err)
	}
	for _, bad := range []string{strings.Repeat("x", 61), "a\nb", "a\tb"} {
		if _, err := s.PutHost("acme", HostConfig{Host: "h", DisplayName: bad}); err == nil {
			t.Errorf("display name %q must be refused", bad)
		}
	}
	// the real name is what agents and instances use: renaming must not change the agent's configuration
	c1, _ := s.AgentConfig("acme", "b4d226bb6dfe")
	s.PutHost("acme", HostConfig{Host: "b4d226bb6dfe", DisplayName: "Another label", Systemd: true})
	c2, _ := s.AgentConfig("acme", "b4d226bb6dfe")
	if c1.Revision != c2.Revision {
		t.Fatal("a display name is only for the UI: it must not make agents restart their collectors")
	}
	// removal is refused while instances are assigned
	s.CreateInstance("acme", InstanceIn{Name: "ks", URL: "https://ks.example.com", Host: "b4d226bb6dfe"})
	if err := s.RemoveHost("acme", "b4d226bb6dfe"); err == nil || !strings.Contains(err.Error(), "ks") {
		t.Fatalf("must name the instances that block removal: %v", err)
	}
	for _, i := range s.ListInstances("acme") {
		s.DeleteInstance("acme", i.ID)
	}
	if err := s.RemoveHost("acme", "b4d226bb6dfe"); err != nil {
		t.Fatal(err)
	}
	g := s.ListHosts("acme")["b4d226bb6dfe"]
	if !g.Removed || g.RemovedAt.IsZero() || g.DisplayName != "" || len(g.LogPaths) != 0 {
		t.Fatalf("tombstone: %+v", g)
	}
	if err := s.RemoveHost("acme", "bad host!"); err == nil {
		t.Fatal("invalid host name")
	}
	// saving settings for the host brings it back
	back, _ := s.PutHost("acme", HostConfig{Host: "b4d226bb6dfe", Systemd: true})
	if back.Removed {
		t.Fatal("saving settings must clear the removal")
	}
}

func TestRemovedInstancesAreRemembered(t *testing.T) {
	s, _ := newReg(t)
	in := InstanceIn{Name: "ks", URL: "https://ks.example.com", Host: "web1"}
	i, err := s.CreateInstance("acme", in)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.GoneInstances("acme")) != 0 {
		t.Fatal("nothing removed yet")
	}
	// removing a managed instance remembers its metric key, so the agent's last reports do not bring it back
	if err := s.DeleteInstance("acme", i.ID); err != nil {
		t.Fatal(err)
	}
	g := s.GoneInstances("acme")
	if len(g) != 1 || g["ks.example.com"].IsZero() {
		t.Fatalf("%v", g)
	}
	if len(s.GoneInstances("globex")) != 0 {
		t.Fatal("another tenant does not see it")
	}
	// adding an instance at the same address brings it back
	i2, err := s.CreateInstance("acme", in)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.GoneInstances("acme")) != 0 {
		t.Fatal("added again: no longer removed")
	}
	// moving an instance to an address that was removed brings that address back too
	s.MarkGone("acme", "old.example.com", "old")
	in.URL = "https://old.example.com"
	if _, err := s.UpdateInstance("acme", i2.ID, in); err != nil {
		t.Fatal(err)
	}
	if _, still := s.GoneInstances("acme")["old.example.com"]; still {
		t.Fatal("an address that is in use again is not 'removed'")
	}
	// the key is checked: it ends up in an identifier
	for _, bad := range []string{"", "a b", "x/y", "a\nb", strings.Repeat("a", 300)} {
		if err := s.MarkGone("acme", bad, ""); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	for _, good := range []string{"ks.example.com", "127.0.0.1:8443", "[::1]:8080", "nc_1.internal"} {
		if err := s.MarkGone("acme", good, ""); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	s.ClearGone("acme", "ks.example.com")
	s.ClearGone("acme", "")
}
