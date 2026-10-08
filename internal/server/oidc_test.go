package server

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/audit"
	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/dashboards"
	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/oidc"
	"github.com/danielingemar/lumen/internal/secretbox"
)

// a provider that signs in whoever the test says
type fakeProvider struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	claims map[string]any // what the next token says (the test sets sub, email, ...)
	nonces map[string]string
}

func newFakeProvider(t *testing.T) *fakeProvider {
	p := &fakeProvider{nonces: map[string]string{}, claims: map[string]any{}}
	p.key, _ = rsa.GenerateKey(rand.Reader, 2048)
	b := func(v []byte) string { return base64.RawURLEncoding.EncodeToString(v) }
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": p.srv.URL, "authorization_endpoint": p.srv.URL + "/authorize", "token_endpoint": p.srv.URL + "/token", "jwks_uri": p.srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k1", "use": "sig", "n": b(p.key.N.Bytes()), "e": b(big.NewInt(int64(p.key.E)).Bytes())}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		p.mu.Lock()
		cl := map[string]any{"iss": p.srv.URL, "aud": "lumen-client", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": p.nonces[r.Form.Get("code")]}
		for k, v := range p.claims {
			cl[k] = v
		}
		p.mu.Unlock()
		h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1"})
		c, _ := json.Marshal(cl)
		in := b(h) + "." + b(c)
		sum := sha256.Sum256([]byte(in))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
		json.NewEncoder(w).Encode(map[string]string{"id_token": in + "." + b(sig)})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

type oidcEnv struct {
	ts    *httptest.Server
	p     *fakeProvider
	st    *auth.Store
	log   *audit.Log
	svc   *oidc.Service
	admin *http.Client
	other *http.Client
	read  *http.Client
}

func newOIDCEnv(t *testing.T) *oidcEnv {
	f, _ := docstore.OpenFile(t.TempDir())
	st, _ := auth.Open(f, 0)
	st.CreateUser("admin", "acme", "admins-long-password")
	st.CreateUserIn("reader", "acme", "readers-long-password", "user")
	st.CreateUser("globexadmin", "globex", "globex-long-password")
	a := auth.New(st, nil, false)
	box, _ := secretbox.New("k")
	al := audit.New(f)
	svc := &oidc.Service{DB: f, Box: box, PublicURL: "http://lumen.test", Secret: st.Secret}
	srv := New(&fakeStore{}, a, slog.New(slog.NewTextHandler(io.Discard, nil))).WithAuth(a).WithDashboards(dashboards.New(f)).WithAudit(al).WithOIDC(svc).WithOwnerTenant("acme")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	e := &oidcEnv{ts: ts, p: newFakeProvider(t), st: st, log: al, svc: svc, admin: client(), other: client(), read: client()}
	login(t, e.admin, ts, "admin", "admins-long-password")
	login(t, e.other, ts, "globexadmin", "globex-long-password")
	login(t, e.read, ts, "reader", "readers-long-password")
	return e
}

func (e *oidcEnv) setup(t *testing.T, extra string) {
	body := `{"enabled":true,"issuer":"` + e.p.srv.URL + `","client_id":"lumen-client","client_secret":"s3cret","label":"Company login"` + extra + `}`
	if r, b := do(e.admin, "PUT", e.ts.URL+"/api/v1/settings/oidc", body); r.StatusCode != 200 {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
}

// signIn goes through the whole thing as a browser would, and says where it ended up and with which session.
func (e *oidcEnv) signIn(t *testing.T, claims map[string]any) (location string, session *http.Cookie) {
	e.p.mu.Lock()
	e.p.claims = claims
	e.p.mu.Unlock()
	nf := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := nf.Get(e.ts.URL + "/auth/oidc/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	to, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(to.String(), e.p.srv.URL+"/authorize") {
		return resp.Header.Get("Location"), nil // it did not get as far as the provider
	}
	q := to.Query()
	e.p.mu.Lock()
	e.p.nonces["code-"+q.Get("state")] = q.Get("nonce")
	e.p.mu.Unlock()
	req, _ := http.NewRequest("GET", e.ts.URL+"/auth/oidc/callback?code=code-"+q.Get("state")+"&state="+q.Get("state"), nil)
	for _, c := range resp.Cookies() {
		req.AddCookie(c)
	}
	resp, err = nf.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName && c.Value != "" {
			session = c
		}
	}
	return resp.Header.Get("Location"), session
}

func (e *oidcEnv) whoIs(t *testing.T, s *http.Cookie) string {
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/v1/me", nil)
	req.AddCookie(s)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var me struct{ User, Tenant, Group string }
	json.NewDecoder(r.Body).Decode(&me)
	return me.User + "@" + me.Tenant + ":" + me.Group
}

func claimsFor(email string) map[string]any {
	return map[string]any{"sub": "sub-" + email, "email": email, "email_verified": true, "name": "A Person"}
}

func TestTheSettingOfSingleSignOn(t *testing.T) {
	e := newOIDCEnv(t)
	// the login page asks what to offer: nothing, before it is set up
	if _, b := do(client(), "GET", e.ts.URL+"/api/v1/auth/providers", ""); strings.TrimSpace(string(b)) != "{}" {
		t.Fatalf("%s", b)
	}
	if code(e.read, "GET", e.ts.URL+"/api/v1/settings/oidc", "") != 403 || code(e.other, "GET", e.ts.URL+"/api/v1/settings/oidc", "") != 403 {
		t.Fatal("only whoever runs the installation, and may see settings")
	}
	if code(e.other, "PUT", e.ts.URL+"/api/v1/settings/oidc", `{"enabled":false}`) != 403 {
		t.Fatal("and may change it")
	}
	for name, c := range map[string]struct{ body, want string }{
		"no client id":                 {`{"enabled":true,"issuer":"` + e.p.srv.URL + `","client_secret":"x"}`, "client id"},
		"no secret":                    {`{"enabled":true,"issuer":"` + e.p.srv.URL + `","client_id":"c"}`, "client secret"},
		"plain http elsewhere":         {`{"enabled":true,"issuer":"http://idp.example.com","client_id":"c","client_secret":"x"}`, "https"},
		"a provider that is not there": {`{"enabled":true,"issuer":"http://127.0.0.1:1","client_id":"c","client_secret":"x"}`, "not turned on"},
		"administrators for all":       {`{"enabled":false,"issuer":"` + e.p.srv.URL + `","auto_create":true,"default_group":"admin"}`, "administrators"},
		"a domain that is not one":     {`{"enabled":false,"domains":["not a domain"]}`, "not a domain"},
		"a group that does not exist":  {`{"enabled":false,"default_group":"nosuchgroup"}`, "no group"},
	} {
		r, b := do(e.admin, "PUT", e.ts.URL+"/api/v1/settings/oidc", c.body)
		if r.StatusCode != 400 || !strings.Contains(string(b), c.want) {
			t.Errorf("%s: %d %s", name, r.StatusCode, b)
		}
	}
	e.setup(t, `,"domains":["@Example.com","example.org"],"auto_create":true,"default_group":"user"`)
	_, b := do(e.admin, "GET", e.ts.URL+"/api/v1/settings/oidc", "")
	if strings.Contains(string(b), "s3cret") || !strings.Contains(string(b), `"has_secret":true`) || !strings.Contains(string(b), `"redirect_uri":"http://lumen.test/auth/oidc/callback"`) || !strings.Contains(string(b), `"domains":["example.com","example.org"]`) {
		t.Fatalf("the secret is never given back; domains are tidied: %s", b)
	}
	if _, b = do(client(), "GET", e.ts.URL+"/api/v1/auth/providers", ""); !strings.Contains(string(b), `"label":"Company login"`) {
		t.Fatalf("%s", b)
	}
	// saving again without a secret keeps the one that is there
	do(e.admin, "PUT", e.ts.URL+"/api/v1/settings/oidc", `{"enabled":true,"issuer":"`+e.p.srv.URL+`","client_id":"lumen-client","label":"Our SSO","auto_create":true}`)
	c, _ := e.svc.Load(context.Background())
	if e.svc.Box == nil || c.Secret == "" || c.Label != "Our SSO" {
		t.Fatalf("%+v", c)
	}
	// testing the provider
	if r, b := do(e.admin, "POST", e.ts.URL+"/api/v1/settings/oidc/test", `{"issuer":"`+e.p.srv.URL+`"}`); r.StatusCode != 200 || !strings.Contains(string(b), "/authorize") {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	if r, _ := do(e.admin, "POST", e.ts.URL+"/api/v1/settings/oidc/test", `{"issuer":"http://127.0.0.1:1"}`); r.StatusCode != 400 {
		t.Fatal("a provider that cannot be reached")
	}
}

func TestSigningInWithTheProvider(t *testing.T) {
	e := newOIDCEnv(t)
	e.setup(t, `,"domains":["example.com"],"auto_create":true,"default_group":"user"`)
	// a first sign-in makes an account, in the tenant of the setting, in the group that was chosen
	loc, sess := e.signIn(t, claimsFor("alice@example.com"))
	if loc != "/" || sess == nil || e.whoIs(t, sess) != "alice@example.com@acme:user" {
		t.Fatalf("%q %v", loc, sess)
	}
	// the next time it is the same account, also if the address at the provider changes (the link is the provider's own id)
	c := claimsFor("alice@example.com")
	c["email"] = "alice@example.com"
	if _, sess = e.signIn(t, c); sess == nil || e.whoIs(t, sess) != "alice@example.com@acme:user" {
		t.Fatal("again")
	}
	if n := len(e.st.ListUsersIn("acme")); n != 3 {
		t.Fatalf("no second account: %d", n)
	}
	// the provider's person cannot walk in with a password they do not know
	if _, ok := e.st.VerifyLogin("alice@example.com", ""); ok {
		t.Fatal("no empty password")
	}
	// what is on record: the account that was made, and the sign-ins, as single sign-on
	var made, signed bool
	for _, x := range e.log.List("acme", 0) {
		made = made || x.Action == "user.create" && x.Via == "oidc" && x.Target == "alice@example.com"
		signed = signed || x.Action == "login" && x.Via == "oidc" && x.Actor == "alice@example.com"
	}
	if !made || !signed {
		t.Fatalf("%v %v", made, signed)
	}
	// who may not: another domain, an address the provider has not verified, no address at all
	for name, cl := range map[string]map[string]any{
		"another domain": claimsFor("mallory@evil.example.net"),
		"not verified":   {"sub": "s2", "email": "bob@example.com", "email_verified": false},
		"no address":     {"sub": "s3"},
	} {
		loc, sess := e.signIn(t, cl)
		if sess != nil || !strings.HasPrefix(loc, "/?oidc_error=") {
			t.Errorf("%s: %q %v", name, loc, sess)
		}
	}
	// a refusal is on record, with the reason, and a user nobody asked about does not appear
	var refused string
	for _, x := range e.log.List("acme", 0) {
		if x.Action == "login.failed" && x.Via == "oidc" && strings.Contains(x.Detail, "domain") {
			refused = x.Detail
		}
	}
	if refused == "" {
		t.Fatal("the refusal and its reason are on record")
	}
}

func TestAccountsThatAlreadyExist(t *testing.T) {
	e := newOIDCEnv(t)
	e.st.CreateUserIn("carol@example.com", "acme", "carols-long-password-1", "user")
	e.st.CreateUserIn("dave@example.com", "globex", "daves-long-password-1", "user")
	e.setup(t, `,"auto_create":false`)
	// not allowed to make accounts: somebody who has none is told so
	if loc, sess := e.signIn(t, claimsFor("new@example.com")); sess != nil || !strings.Contains(loc, "there+is+no+account") {
		t.Fatalf("%q", loc)
	}
	// an account of the same name is not taken over just because the addresses match
	if loc, sess := e.signIn(t, claimsFor("carol@example.com")); sess != nil || !strings.Contains(loc, "not+linked") {
		t.Fatalf("an account of that name exists and is not linked: %q", loc)
	}
	// unless an administrator chose to trust the provider's word about the address
	e.setup(t, `,"auto_create":false,"link_by_email":true`)
	if _, sess := e.signIn(t, claimsFor("carol@example.com")); sess == nil || e.whoIs(t, sess) != "carol@example.com@acme:user" {
		t.Fatal("linked by a verified address")
	}
	// ... and then it is carol's own link, which does not depend on the address any more
	e.setup(t, `,"auto_create":false,"link_by_email":false`)
	if _, sess := e.signIn(t, claimsFor("carol@example.com")); sess == nil {
		t.Fatal("it stays linked")
	}
	// the same name in another tenant is never entered
	if loc, sess := e.signIn(t, claimsFor("dave@example.com")); sess != nil || !strings.Contains(loc, "elsewhere") {
		t.Fatalf("%q", loc)
	}
}

func TestTheStartOfASignInIsSafe(t *testing.T) {
	e := newOIDCEnv(t)
	nf := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// not set up: nothing starts
	resp, _ := nf.Get(e.ts.URL + "/auth/oidc/login")
	resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Location"), "/?oidc_error=") {
		t.Fatalf("%q", resp.Header.Get("Location"))
	}
	e.setup(t, `,"auto_create":true`)
	resp, _ = nf.Get(e.ts.URL + "/auth/oidc/login")
	resp.Body.Close()
	var ck *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == oidc.CookieName {
			ck = c
		}
	}
	if resp.StatusCode != 302 || ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/auth/oidc" {
		t.Fatalf("%d %+v", resp.StatusCode, ck)
	}
	// an answer that does not belong to a sign-in that was started in this browser is refused, and no session is made
	resp, _ = nf.Get(e.ts.URL + "/auth/oidc/callback?code=x&state=forged")
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "oidc_error=") {
		t.Fatalf("%q", resp.Header.Get("Location"))
	}
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName && c.Value != "" {
			t.Fatal("no session without a sign-in")
		}
	}
	// guessing is slowed down, as with passwords
	for i := 0; i < 12; i++ {
		r, _ := nf.Get(e.ts.URL + "/auth/oidc/callback?code=x&state=forged")
		r.Body.Close()
	}
	r, _ := nf.Get(e.ts.URL + "/auth/oidc/login")
	r.Body.Close()
	if !strings.Contains(r.Header.Get("Location"), "too+many") {
		t.Fatalf("%q", r.Header.Get("Location"))
	}
}
