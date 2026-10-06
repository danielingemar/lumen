package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
)

var t0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func issue(t *testing.T, priv ed25519.PrivateKey, keyID string, mod func(*Payload)) []byte {
	p := Payload{ID: "L-1", Customer: "ACME AB", Editions: []string{Enterprise}, Issued: t0, Expires: t0.AddDate(1, 0, 0)}
	if mod != nil {
		mod(&p)
	}
	b, err := Sign(priv, keyID, p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func keypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := NewKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestRoundTripAndTampering(t *testing.T) {
	pub, priv := keypair(t)
	keys := Keys{"main": pub}
	raw := issue(t, priv, "main", func(p *Payload) {
		p.Editions = []string{Operator, Enterprise}
		p.Limits = Limits{Hosts: 50, Tenants: 10}
	})
	l, err := Parse(raw, keys)
	if err != nil || l.Customer != "ACME AB" || l.ID != "L-1" || !l.Has(Enterprise) || !l.Has(Operator) || l.Limits.Hosts != 50 || l.KeyID != "main" || !l.Expires.Equal(t0.AddDate(1, 0, 0)) {
		t.Fatalf("%+v %v", l, err)
	}
	// anything changed after signing is caught: here the customer is edited, and an expiry is extended
	var f File
	json.Unmarshal(raw, &f)
	body, _ := base64.StdEncoding.DecodeString(f.Payload)
	forged := strings.Replace(string(body), "ACME AB", "EVIL AB", 1)
	f.Payload = base64.StdEncoding.EncodeToString([]byte(forged))
	fb, _ := json.Marshal(f)
	if _, err := Parse(fb, keys); err == nil || !strings.Contains(err.Error(), "signature does not match") {
		t.Fatalf("an edited licence must be refused: %v", err)
	}
	longer := strings.Replace(string(body), `"expires":"`+t0.AddDate(1, 0, 0).Format(time.RFC3339), `"expires":"`+t0.AddDate(9, 0, 0).Format(time.RFC3339), 1)
	f.Payload = base64.StdEncoding.EncodeToString([]byte(longer))
	fb, _ = json.Marshal(f)
	if _, err := Parse(fb, keys); err == nil {
		t.Fatal("a longer expiry date cannot be written in by hand")
	}
	// another publisher's key, an unknown key id, no keys at all
	otherPub, otherPriv := keypair(t)
	if _, err := Parse(issue(t, otherPriv, "main", nil), keys); err == nil || !strings.Contains(err.Error(), "signature does not match") {
		t.Fatalf("a licence signed by someone else's key: %v", err)
	}
	if _, err := Parse(issue(t, otherPriv, "other", nil), keys); err == nil || !strings.Contains(err.Error(), `"other", which this build of Lumen does not trust`) {
		t.Fatalf("%v", err)
	}
	if _, err := Parse(raw, Keys{}); err == nil || !strings.Contains(err.Error(), "trusts no licence keys") {
		t.Fatalf("a Community build says why it cannot verify: %v", err)
	}
	_ = otherPub
	for name, data := range map[string][]byte{"empty": nil, "text": []byte("hello"), "json": []byte(`{"a":1}`), "wrong format": []byte(`{"format":"x","key_id":"main","payload":"a","signature":"b"}`), "huge": []byte(strings.Repeat("a", 20000)), "damaged": []byte(`{"format":"lumen-license-1","key_id":"main","payload":"!!!","signature":"???"}`)} {
		if _, err := Parse(data, keys); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s must be refused with a readable reason: %v", name, err)
		}
	}
}

func TestSigningRefusesNonsense(t *testing.T) {
	_, priv := keypair(t)
	base := Payload{ID: "L", Customer: "A", Editions: []string{Enterprise}, Issued: t0, Expires: t0.AddDate(0, 1, 0)}
	for name, mod := range map[string]func(*Payload){
		"no customer": func(p *Payload) { p.Customer = " " }, "multiline customer": func(p *Payload) { p.Customer = "a\nb" },
		"no edition": func(p *Payload) { p.Editions = nil }, "unknown edition": func(p *Payload) { p.Editions = []string{"platinum"} },
		"expires before issue": func(p *Payload) { p.Expires = t0.Add(-time.Hour) }, "negative": func(p *Payload) { p.Limits.Hosts = -1 }, "no id": func(p *Payload) { p.ID = "" },
	} {
		p := base
		mod(&p)
		if _, err := Sign(priv, "k", p); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Sign(priv, "bad id", base); err == nil {
		t.Fatal("key id")
	}
}

func TestStatesAndGrace(t *testing.T) {
	l := License{Payload: Payload{Editions: []string{Enterprise}, Issued: t0, Expires: t0.AddDate(1, 0, 0)}}
	exp := l.Expires
	for _, c := range []struct {
		at   time.Time
		want State
		days int
	}{
		{t0, StateValid, 365}, {exp.Add(-31 * 24 * time.Hour), StateValid, 31}, {exp.Add(-29 * 24 * time.Hour), StateExpiring, 29}, {exp.Add(-time.Hour), StateExpiring, 1}, {exp, StateGrace, 0},
		{exp.Add(time.Hour), StateGrace, -1}, {exp.Add(29 * 24 * time.Hour), StateGrace, -29}, {exp.Add(31 * 24 * time.Hour), StateExpired, -31},
	} {
		if got := l.State(c.at); got != c.want {
			t.Errorf("at %v: %s, want %s", c.at, got, c.want)
		}
		if got := l.DaysLeft(c.at); got != c.days {
			t.Errorf("at %v: %d days, want %d", c.at, got, c.days)
		}
	}
	if !l.Active(exp.Add(29*24*time.Hour)) || l.Active(exp.Add(31*24*time.Hour)) {
		t.Fatal("Enterprise features work through the 30-day grace period and stop after it")
	}
}

func TestWarningsAreSoft(t *testing.T) {
	l := License{Payload: Payload{Limits: Limits{Hosts: 50, Tenants: 10}}}
	if w := l.Warnings(10, 3); len(w) != 0 {
		t.Fatalf("%v", w)
	}
	if w := l.Warnings(45, 3); len(w) != 1 || !strings.Contains(w[0], "45 of the 50 hosts") {
		t.Fatalf("a warning at 90 percent: %v", w)
	}
	if w := l.Warnings(60, 12); len(w) != 2 || !strings.Contains(w[0], "Nothing is blocked") {
		t.Fatalf("over the limit: a warning that says nothing is blocked: %v", w)
	}
	if w := (License{}).Warnings(1000, 1000); len(w) != 0 {
		t.Fatal("no limits: no warnings")
	}
}

func TestKeyFiles(t *testing.T) {
	pub, _ := keypair(t)
	fsys := fstest.MapFS{"keys/main.pub": {Data: []byte(EncodeKey(pub) + "\n")}, "keys/README.md": {Data: []byte("x")}, "keys/old.pub": {Data: []byte(EncodeKey(pub))}}
	k, err := LoadKeys(fsys, "keys")
	if err != nil || len(k) != 2 || !k["main"].Equal(pub) {
		t.Fatalf("%v %v", k, err)
	}
	if _, err := LoadKeys(fstest.MapFS{"keys/x.pub": {Data: []byte("not a key")}}, "keys"); err == nil {
		t.Fatal("a damaged key file is an error, not silently ignored")
	}
	if _, err := ParsePrivateKey("zzz"); err == nil {
		t.Fatal("private key")
	}
	// What is built in is the publisher's PUBLIC key(s), if any have been added (a plain open-source build has none, and then
	// verifies no licence). Whatever is there must be usable, and a private key must never end up in the program.
	keys := EmbeddedKeys() // panics if a built-in key is damaged
	for id, k := range keys {
		if len(k) != ed25519.PublicKeySize {
			t.Errorf("the built-in key %q is not a public key", id)
		}
	}
	ents, err := keyFS.ReadDir("keys")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".key") || strings.Contains(e.Name(), "private") {
			t.Errorf("%s: a private key must never be built into the program", e.Name())
		}
	}
	// a licence signed by a key nobody trusts is never accepted, whatever keys are built in
	_, stranger := keypair(t)
	forged := issue(t, stranger, "main", nil)
	if _, err := Parse(forged, keys); err == nil {
		t.Fatal("a licence signed with an unknown key must not verify")
	}
}

type clk struct{ t time.Time }

func (c *clk) now() time.Time { return c.t }

func newManager(t *testing.T, file string) (*Manager, ed25519.PrivateKey, *clk, docstore.Backend) {
	pub, priv := keypair(t)
	db, err := docstore.OpenFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := &clk{t0}
	m := NewManager(db, Keys{"main": pub}, file)
	m.Now = c.now
	m.Load()
	return m, priv, c, db
}

func TestManagerLifecycle(t *testing.T) {
	m, priv, c, db := newManager(t, "")
	if m.State() != StateNone || m.Allows(Enterprise) || !m.Allows(Community) || !m.Allows("") {
		t.Fatal("without a licence: Community only")
	}
	if i := m.Info(); i.State != StateNone || i.Trusted != 1 || i.Customer != "" {
		t.Fatalf("%+v", i)
	}
	// a licence that does not verify is refused and changes nothing
	if _, err := m.Set([]byte("garbage"), "anna"); !errors.Is(err, ErrInvalid) || m.State() != StateNone {
		t.Fatalf("%v", err)
	}
	if _, err := m.Set(issue(t, priv, "unknown-key", nil), "anna"); err == nil {
		t.Fatal("unknown key")
	}
	// a good one
	l, err := m.Set(issue(t, priv, "main", nil), "anna")
	if err != nil || l.Customer != "ACME AB" || m.State() != StateValid || !m.Allows(Enterprise) || m.Allows(Operator) {
		t.Fatalf("%v %v: the licence names Enterprise only", err, m.State())
	}
	i := m.Info()
	if i.State != StateValid || i.Customer != "ACME AB" || i.Source != "stored" || i.DaysLeft != 365 || i.GraceEnds == "" || len(i.Editions) != 1 {
		t.Fatalf("%+v", i)
	}
	// it survives a restart (a new manager on the same store)
	m2 := NewManager(db, m.keys, "")
	m2.Now = c.now
	m2.Load()
	if m2.State() != StateValid || !m2.Allows(Enterprise) {
		t.Fatal("kept across restarts")
	}
	// time passes: expiring, grace (still works), expired (off), and everything Community keeps working
	for _, s := range []struct {
		at      time.Time
		state   State
		allowed bool
	}{
		{t0.AddDate(1, 0, -10), StateExpiring, true}, {t0.AddDate(1, 0, 10), StateGrace, true}, {t0.AddDate(1, 0, 40), StateExpired, false},
	} {
		c.t = s.at
		if m.State() != s.state || m.Allows(Enterprise) != s.allowed || !m.Allows(Community) {
			t.Errorf("at %v: %s allowed=%v, want %s %v", s.at, m.State(), m.Allows(Enterprise), s.state, s.allowed)
		}
	}
	// a renewal replaces the old licence
	c.t = t0
	if _, err := m.Set(issue(t, priv, "main", func(p *Payload) { p.ID = "L-2"; p.Editions = []string{Enterprise, Operator} }), "anna"); err != nil || !m.Allows(Operator) || m.Info().ID != "L-2" {
		t.Fatal("renewal")
	}
	if err := m.Remove(); err != nil || m.State() != StateNone || m.Allows(Enterprise) {
		t.Fatalf("removing goes back to Community: %v", err)
	}
	if err := m.Remove(); err != nil {
		t.Fatal("removing twice is fine")
	}
}

func TestLicenceFileOverridesAndIsPickedUp(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "lumen.license")
	m, priv, c, _ := newManager(t, file)
	if m.State() != StateNone {
		t.Fatal("a missing file is not an error")
	}
	os.WriteFile(file, issue(t, priv, "main", nil), 0o600)
	m.Load()
	if m.State() != StateValid || m.Info().Source != "file" {
		t.Fatalf("%+v", m.Info())
	}
	// a licence that comes from a file is renewed by replacing the file, not in the interface
	if _, err := m.Set(issue(t, priv, "main", nil), "anna"); err == nil || !strings.Contains(err.Error(), "replace that file") {
		t.Fatalf("%v", err)
	}
	if err := m.Remove(); err == nil || !strings.Contains(err.Error(), "remove that file") {
		t.Fatalf("%v", err)
	}
	os.WriteFile(file, issue(t, priv, "main", func(p *Payload) { p.ID = "L-9" }), 0o600)
	m.Load()
	if m.Info().ID != "L-9" {
		t.Fatal("a renewed file is picked up")
	}
	// a damaged file: the state says invalid, with the reason, and Enterprise is off
	os.WriteFile(file, []byte("oops"), 0o600)
	m.Load()
	if i := m.Info(); i.State != StateInvalid || i.Problem == "" || m.Allows(Enterprise) {
		t.Fatalf("%+v", i)
	}
	_ = c
}
