package auth

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/docstore/estest"
)

func init() { Iterations = 1000 } // fast tests; production default is 600,000

// eachStore runs a test against both backends. mk() returns a NEW Store on the same underlying
// storage, which simulates a second process (the CLI) sharing it with the server.
func eachStore(t *testing.T, fn func(t *testing.T, mk func() *Store, name string)) {
	t.Run("file", func(t *testing.T) {
		dir := t.TempDir()
		fn(t, func() *Store {
			s, err := OpenDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			return s
		}, "file")
	})
	t.Run("elasticsearch(fake)", func(t *testing.T) {
		ts, _ := estest.New()
		t.Cleanup(ts.Close)
		es := docstore.NewES(docstore.ESConfig{URL: ts.URL, Prefix: "t"})
		if err := es.EnsureIndices(context.Background()); err != nil {
			t.Fatal(err)
		}
		fn(t, func() *Store {
			s, err := Open(es, 5*time.Second) // caching on: local writes must still take effect immediately
			if err != nil {
				t.Fatal(err)
			}
			return s
		}, "es")
	})
}

func TestPBKDF2Vectors(t *testing.T) { // reference values from Python's hashlib.pbkdf2_hmac
	cases := []struct {
		pw, salt string
		iter, n  int
		want     string
	}{
		{"password", "salt", 1, 32, "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{"password", "salt", 4096, 32, "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
		{"passwordPASSWORDpassword", "saltSALTsaltSALTsaltSALTsaltSALTsalt", 4096, 40, "348c89dbcbd32b2f32d814b8116e84cf2b17347ebc1800181c4e2a1fb8dd53e1c635518c7dac47e9"},
	}
	for _, c := range cases {
		if got := hex.EncodeToString(pbkdf2([]byte(c.pw), []byte(c.salt), c.iter, c.n)); got != c.want {
			t.Errorf("pbkdf2(%q,%q,%d): got %s", c.pw, c.salt, c.iter, got)
		}
	}
}

func TestPasswordHashing(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h, "correct") || !VerifyPassword("correct horse battery", h) || VerifyPassword("wrong horse battery", h) {
		t.Fatal("hash/verify broken")
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("salts must differ")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short passwords must be rejected")
	}
	for _, bad := range []string{"", "x", "pbkdf2-sha256$0$aa$bb", "md5$1$aa$bb", "pbkdf2-sha256$99999999999$aa$bb"} {
		if VerifyPassword("x", bad) {
			t.Fatalf("malformed hash %q must not verify", bad)
		}
	}
}

func TestStoreUsersAndKeys(t *testing.T) {
	eachStore(t, func(t *testing.T, mk func() *Store, name string) {
		s := mk()
		if s.HasCredentials() {
			t.Fatal("fresh store has no credentials")
		}
		if err := s.CreateUser("Alice", "acme", "a-long-password"); err != nil {
			t.Fatal(err)
		}
		if s.CreateUser("alice", "acme", "another-long-pw") == nil {
			t.Fatal("duplicate (case-insensitive) user must fail")
		}
		if _, ok := s.VerifyLogin("ALICE", "a-long-password"); !ok {
			t.Fatal("login is case-insensitive on username")
		}
		if _, ok := s.VerifyLogin("alice", "nope-nope-nope"); ok {
			t.Fatal("wrong password must fail")
		}
		if _, ok := s.VerifyLogin("ghost", "a-long-password"); ok {
			t.Fatal("unknown user must fail")
		}
		plain, k, err := s.CreateKey("acme", "vm1")
		if err != nil {
			t.Fatal(err)
		}
		if tenant, ok := s.LookupKey(plain); !ok || tenant != "acme" {
			t.Fatal("a new key must work immediately")
		}
		if _, ok := s.LookupKey(plain + "x"); ok {
			t.Fatal("wrong key must fail")
		}
		// a second process (the CLI) shares the storage
		s2 := mk()
		if err := s2.CreateUser("bob", "acme", "bobs-long-password"); err != nil {
			t.Fatal(err)
		}
		if _, ok := s.GetUser("bob"); !ok {
			t.Fatal("the running server must see users added by the CLI")
		}
		if err := s.DeleteKey("other-tenant", k.ID); err == nil {
			t.Fatal("a tenant must not delete another tenant's key")
		}
		if len(s.ListKeys("other-tenant")) != 0 || len(s.ListKeys("acme")) != 1 {
			t.Fatal("keys must be tenant-scoped")
		}
		if got := s.ListKeys("acme")[0]; got.Hash != "" || got.Prefix != plain[:8] || got.Name != "vm1" {
			t.Fatalf("listing must not expose the hash: %+v", got)
		}
		if err := s.DeleteKey("acme", k.ID); err != nil {
			t.Fatal(err)
		}
		if _, ok := s.LookupKey(plain); ok {
			t.Fatal("a deleted key must stop working immediately, even with caching on")
		}
		if err := s.DeleteUser("bob"); err != nil {
			t.Fatal(err)
		}
		if _, ok := s.GetUser("bob"); ok {
			t.Fatal("a deleted user must be gone immediately")
		}
		for _, bad := range []string{"", "a b", "../x", strings.Repeat("a", 65), "UPPER;drop"} {
			if _, err := NormalizeUser(bad); err == nil {
				t.Errorf("username %q must be rejected", bad)
			}
		}
	})
}

func TestFileStoreNeverWritesSecretsInClear(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenDir(dir)
	s.CreateUser("alice", "acme", "a-long-password")
	plain, _, _ := s.CreateKey("acme", "vm1")
	raw, _ := os.ReadFile(filepath.Join(dir, "documents.json"))
	if strings.Contains(string(raw), plain) || strings.Contains(string(raw), "a-long-password") {
		t.Fatal("plain key or password must never be written to disk")
	}
	if st, _ := os.Stat(filepath.Join(dir, "documents.json")); st.Mode().Perm() != 0o600 {
		t.Fatalf("documents.json must be 0600, is %v", st.Mode().Perm())
	}
	if len(s.Secret()) < 32 {
		t.Fatal("session secret missing")
	}
	s2, _ := OpenDir(dir)
	if string(s2.Secret()) != string(s.Secret()) {
		t.Fatal("session secret must persist across restarts")
	}
}

func TestImportLegacyAuthFile(t *testing.T) {
	dir := t.TempDir()
	h, _ := HashPassword("old-password-123")
	legacy := `{"session_secret":"x","users":{"old":{"name":"old","tenant":"acme","hash":"` + h + `","created":"2026-01-01T00:00:00Z"}},` +
		`"keys":{"k1":{"id":"k1","name":"vm","tenant":"acme","prefix":"lmn_abcd","hash":"` + hashKey("lmn_oldkey") + `","created":"2026-01-01T00:00:00Z"}}}`
	os.WriteFile(filepath.Join(dir, "auth.json"), []byte(legacy), 0o600)
	s, _ := OpenDir(dir)
	n, err := ImportLegacy(s, dir)
	if err != nil || n != 2 {
		t.Fatalf("import: n=%d err=%v", n, err)
	}
	if _, ok := s.VerifyLogin("old", "old-password-123"); !ok {
		t.Fatal("imported user must be able to log in")
	}
	if tenant, ok := s.LookupKey("lmn_oldkey"); !ok || tenant != "acme" {
		t.Fatal("imported key must still work")
	}
	if _, err := os.Stat(filepath.Join(dir, "auth.json.migrated")); err != nil {
		t.Fatal("legacy file must be renamed so it is not imported twice")
	}
}

type httptestReq struct{ r *http.Request }

func newReq(tok string) *httptestReq {
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: tok})
	return &httptestReq{r}
}

func TestSessions(t *testing.T) {
	eachStore(t, func(t *testing.T, mk func() *Store, name string) {
		s := mk()
		s.CreateUser("alice", "acme", "a-long-password")
		a := New(s, nil, false)
		u, _ := s.GetUser("alice")
		tok := a.IssueSession(u)
		if id, err := a.Authenticate(newReq(tok).r); err != nil || id.Tenant != "acme" || id.User != "alice" || !id.Session || id.ViaKey {
			t.Fatalf("valid session rejected: %+v %v", id, err)
		}
		if _, err := a.Authenticate(newReq(tok[:len(tok)-2] + "xx").r); err == nil {
			t.Fatal("tampered signature must fail")
		}
		msg, sig, _ := strings.Cut(tok, ".")
		if _, err := a.Authenticate(newReq(msg + "A." + sig).r); err == nil {
			t.Fatal("modified payload must fail")
		}
		s.SetPassword("alice", "a-new-long-password")
		if _, err := a.Authenticate(newReq(tok).r); err == nil {
			t.Fatal("changing the password must end existing sessions")
		}
		u, _ = s.GetUser("alice")
		tok2 := a.IssueSession(u)
		s.DeleteUser("alice")
		if _, err := a.Authenticate(newReq(tok2).r); err == nil {
			t.Fatal("deleted user's session must fail")
		}
		if _, err := a.Authenticate(httptest.NewRequest("GET", "/", nil)); err == nil {
			t.Fatal("no credentials and not dev mode must fail")
		}
		if id, err := New(s, nil, true).Authenticate(httptest.NewRequest("GET", "/", nil)); err != nil || id.Tenant != "default" {
			t.Fatal("explicit dev mode allows anonymous access")
		}
	})
}

func TestAPIKeySessions(t *testing.T) {
	eachStore(t, func(t *testing.T, mk func() *Store, name string) {
		s := mk()
		plain, k, _ := s.CreateKey("acme", "vm1")
		a := New(s, map[string]string{"envkey-1234567890": "globex"}, false)
		tok, tenant, ok := a.LoginWithKey(plain)
		if !ok || tenant != "acme" {
			t.Fatal("stored key must log in")
		}
		if id, err := a.Authenticate(newReq(tok).r); err != nil || id.Tenant != "acme" || !id.Session || !id.ViaKey || id.User != "" {
			t.Fatalf("key session: %+v %v", id, err)
		}
		etok, etenant, ok := a.LoginWithKey("envkey-1234567890")
		if !ok || etenant != "globex" {
			t.Fatal("bootstrap env key must log in")
		}
		if id, err := a.Authenticate(newReq(etok).r); err != nil || id.Tenant != "globex" || !id.ViaKey {
			t.Fatalf("env key session: %+v %v", id, err)
		}
		if _, _, ok := a.LoginWithKey("not-a-key"); ok {
			t.Fatal("unknown key must fail")
		}
		s.DeleteKey("acme", k.ID)
		if _, err := a.Authenticate(newReq(tok).r); err == nil {
			t.Fatal("deleting the key must end sessions opened with it")
		}
	})
}

func TestEnvKeysAndLimiter(t *testing.T) {
	s, _ := OpenDir(t.TempDir())
	a := New(s, map[string]string{"envkey": "acme"}, false)
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer envkey")
	if id, err := a.Authenticate(r); err != nil || id.Tenant != "acme" || id.Session {
		t.Fatalf("env key: %+v %v", id, err)
	}
	r.Header.Set("Authorization", "Bearer wrong")
	if _, err := a.Authenticate(r); err == nil {
		t.Fatal("wrong key must fail even if dev mode would allow anonymous access")
	}
	l := NewLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if l.Blocked("u") {
			t.Fatal("blocked too early")
		}
		l.Fail("u")
	}
	if !l.Blocked("u") || l.Blocked("other") {
		t.Fatal("limiter must block only the failing username")
	}
	l.Reset("u")
	if l.Blocked("u") {
		t.Fatal("reset must unblock")
	}
}
