package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/secretbox"
)

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// idp is a provider that can be told what to say.
type idp struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex

	rsaKeys map[string]*rsa.PrivateKey
	ecKey   *ecdsa.PrivateKey
	listed  []string // key ids in the key list

	authModes []string
	issuerSay string // what discovery says the issuer is (empty = the real one)

	// the last token request
	gotVerifier, gotBasicUser, gotBasicPass, gotPostSecret, gotCode string
	jwksFetches                                                     int
	mint                                                            func(nonce string) (string, int, string) // token, status, body override
}

func newIdP(t *testing.T) *idp {
	p := &idp{t: t, rsaKeys: map[string]*rsa.PrivateKey{}}
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	p.rsaKeys["k1"] = k
	p.ecKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p.listed = []string{"k1", "ec1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		iss := p.srv.URL
		if p.issuerSay != "" {
			iss = p.issuerSay
		}
		doc := map[string]any{"issuer": iss, "authorization_endpoint": p.srv.URL + "/authorize", "token_endpoint": p.srv.URL + "/token", "jwks_uri": p.srv.URL + "/jwks"}
		if p.authModes != nil {
			doc["token_endpoint_auth_methods_supported"] = p.authModes
		}
		json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.jwksFetches++
		var keys []map[string]string
		for _, id := range p.listed {
			if id == "ec1" {
				keys = append(keys, map[string]string{"kty": "EC", "kid": "ec1", "use": "sig", "crv": "P-256", "x": b64u(p.ecKey.X.FillBytes(make([]byte, 32))), "y": b64u(p.ecKey.Y.FillBytes(make([]byte, 32)))})
			} else if k := p.rsaKeys[id]; k != nil {
				keys = append(keys, map[string]string{"kty": "RSA", "kid": id, "use": "sig", "n": b64u(k.N.Bytes()), "e": b64u(big.NewInt(int64(k.E)).Bytes())})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		p.mu.Lock()
		p.gotVerifier, p.gotCode, p.gotPostSecret = r.Form.Get("code_verifier"), r.Form.Get("code"), r.Form.Get("client_secret")
		p.gotBasicUser, p.gotBasicPass, _ = r.BasicAuth()
		mint := p.mint
		p.mu.Unlock()
		tok, status, body := mint(p.nonceFor(r.Form.Get("code")))
		if body != "" {
			w.WriteHeader(status)
			w.Write([]byte(body))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id_token": tok, "access_token": "x", "token_type": "Bearer"})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

var nonces sync.Map // code -> nonce

func (p *idp) nonceFor(code string) string { v, _ := nonces.Load(code); s, _ := v.(string); return s }

func (p *idp) tokenRS256(kid string, key *rsa.PrivateKey, claims map[string]any) string {
	return p.sign("RS256", kid, claims, func(sum []byte) []byte {
		s, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum)
		return s
	})
}

func (p *idp) tokenES256(claims map[string]any) string {
	return p.sign("ES256", "ec1", claims, func(sum []byte) []byte {
		r, s, _ := ecdsa.Sign(rand.Reader, p.ecKey, sum)
		return append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	})
}

func (p *idp) sign(alg, kid string, claims map[string]any, sig func([]byte) []byte) string {
	h, _ := json.Marshal(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"})
	c, _ := json.Marshal(claims)
	in := b64u(h) + "." + b64u(c)
	sum := sha256.Sum256([]byte(in))
	return in + "." + b64u(sig(sum[:]))
}

func (p *idp) claims(nonce string, now time.Time) map[string]any {
	return map[string]any{"iss": p.srv.URL, "sub": "user-1", "aud": "lumen-client", "exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "nonce": nonce, "email": "Alice@Example.com", "email_verified": true, "name": "Alice A"}
}

type env struct {
	p   *idp
	svc *Service
	cfg Config
	now time.Time
	db  docstore.Backend
}

func newEnv(t *testing.T) *env {
	p := newIdP(t)
	db, _ := docstore.OpenFile(t.TempDir())
	box, _ := secretbox.New("test-key")
	e := &env{p: p, db: db, now: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	e.svc = &Service{DB: db, Box: box, PublicURL: "https://lumen.example.com", Secret: func() []byte { return []byte("session-secret-0123456789") }, Now: func() time.Time { return e.now }}
	e.cfg = Config{Enabled: true, Issuer: p.srv.URL, ClientID: "lumen-client", Tenant: "acme"}
	e.svc.Save(context.Background(), e.cfg, "the-client-secret")
	e.cfg, _ = e.svc.Load(context.Background())
	p.mint = func(nonce string) (string, int, string) {
		return p.tokenRS256("k1", p.rsaKeys["k1"], p.claims(nonce, e.now)), 200, ""
	}
	return e
}

// begin starts a sign-in and returns the cookie, the authorize address and the parameters in it.
func (e *env) begin(t *testing.T) (*http.Cookie, url.Values) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/auth/oidc/login", nil)
	to, err := e.svc.Begin(context.Background(), w, r, e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(to)
	q := u.Query()
	nonces.Store("code-"+q.Get("state"), q.Get("nonce"))
	var ck *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName {
			ck = c
		}
	}
	return ck, q
}

func (e *env) finish(ck *http.Cookie, query url.Values) (Identity, error) {
	r := httptest.NewRequest("GET", "/auth/oidc/callback?"+query.Encode(), nil)
	if ck != nil {
		r.AddCookie(ck)
	}
	return e.svc.Finish(context.Background(), httptest.NewRecorder(), r, e.cfg)
}

func (e *env) ok(t *testing.T) (Identity, error) {
	ck, q := e.begin(t)
	return e.finish(ck, url.Values{"code": {"code-" + q.Get("state")}, "state": {q.Get("state")}})
}

func TestASignInThatIsInOrder(t *testing.T) {
	e := newEnv(t)
	ck, q := e.begin(t)
	if q.Get("response_type") != "code" || q.Get("client_id") != "lumen-client" || q.Get("redirect_uri") != "https://lumen.example.com/auth/oidc/callback" || q.Get("scope") != "openid email profile" || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("nonce") == "" || q.Get("code_challenge") == "" {
		t.Fatalf("the address the browser is sent to: %v", q)
	}
	if !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || !ck.Secure || ck.Path != "/auth/oidc" || ck.MaxAge != 600 {
		t.Fatalf("the cookie of a sign-in that is under way: %+v", ck)
	}
	id, err := e.finish(ck, url.Values{"code": {"code-" + q.Get("state")}, "state": {q.Get("state")}})
	if err != nil || id.Subject != "user-1" || id.Email != "alice@example.com" || id.EmailVerified == nil || !*id.EmailVerified || id.Name != "Alice A" {
		t.Fatalf("%+v %v", id, err)
	}
	// the secret of the application is sent the standard way, and the PKCE proof matches what was promised at the start
	chal := sha256.Sum256([]byte(e.p.gotVerifier))
	if b64u(chal[:]) != q.Get("code_challenge") {
		t.Fatal("the code_verifier must hash to the code_challenge that was sent at the start")
	}
	if e.p.gotBasicUser != "lumen-client" || e.p.gotBasicPass != "the-client-secret" || e.p.gotPostSecret != "" {
		t.Fatalf("basic: %q %q post:%q", e.p.gotBasicUser, e.p.gotBasicPass, e.p.gotPostSecret)
	}
	// a provider that only takes the secret in the body gets it there
	e.p.authModes = []string{"client_secret_post"}
	e.svc.disc = nil
	if _, err := e.ok(t); err != nil || e.p.gotPostSecret != "the-client-secret" || e.p.gotBasicUser != "" {
		t.Fatalf("%v post:%q basic:%q", err, e.p.gotPostSecret, e.p.gotBasicUser)
	}
	// ES256 works too
	e.p.mint = func(nonce string) (string, int, string) { return e.p.tokenES256(e.p.claims(nonce, e.now)), 200, "" }
	if id, err := e.ok(t); err != nil || id.Subject != "user-1" {
		t.Fatalf("%v", err)
	}
}

func TestASignInThatIsNotInOrderIsRefused(t *testing.T) {
	e := newEnv(t)
	wrongKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	mintWith := func(f func(c map[string]any)) func(string) (string, int, string) {
		return func(n string) (string, int, string) {
			c := e.p.claims(n, e.now)
			f(c)
			return e.p.tokenRS256("k1", e.p.rsaKeys["k1"], c), 200, ""
		}
	}
	cases := map[string]struct {
		mint func(string) (string, int, string)
		want string
	}{
		"another issuer":          {mintWith(func(c map[string]any) { c["iss"] = "https://evil.example.com" }), "comes from"},
		"another application":     {mintWith(func(c map[string]any) { c["aud"] = "someone-else" }), "another application"},
		"many audiences, no azp":  {mintWith(func(c map[string]any) { c["aud"] = []string{"lumen-client", "other"} }), "azp"},
		"expired":                 {mintWith(func(c map[string]any) { c["exp"] = e.now.Add(-2 * time.Minute).Unix() }), "expired"},
		"from the future":         {mintWith(func(c map[string]any) { c["iat"] = e.now.Add(10 * time.Minute).Unix() }), "future"},
		"not valid yet":           {mintWith(func(c map[string]any) { c["nbf"] = e.now.Add(10 * time.Minute).Unix() }), "not valid yet"},
		"another sign-in's nonce": {mintWith(func(c map[string]any) { c["nonce"] = "somebody-elses" }), "nonce"},
		"no nonce":                {mintWith(func(c map[string]any) { delete(c, "nonce") }), "nonce"},
		"no subject":              {mintWith(func(c map[string]any) { delete(c, "sub") }), "who signed in"},
		"no expiry":               {mintWith(func(c map[string]any) { delete(c, "exp") }), "expired"},
		"signed by another key": {func(n string) (string, int, string) {
			return e.p.tokenRS256("k1", wrongKey, e.p.claims(n, e.now)), 200, ""
		}, "signature"},
		"a key that is not listed": {func(n string) (string, int, string) {
			return e.p.tokenRS256("nope", e.p.rsaKeys["k1"], e.p.claims(n, e.now)), 200, ""
		}, "does not list"},
		"no signature (alg none)": {func(n string) (string, int, string) {
			h := b64u([]byte(`{"alg":"none"}`))
			c, _ := json.Marshal(e.p.claims(n, e.now))
			return h + "." + b64u(c) + ".", 200, ""
		}, "not accepted"},
		"a shared secret (HS256) made from the public key": {func(n string) (string, int, string) {
			h := b64u([]byte(`{"alg":"HS256","kid":"k1"}`))
			c, _ := json.Marshal(e.p.claims(n, e.now))
			m := hmac.New(sha256.New, e.p.rsaKeys["k1"].N.Bytes())
			m.Write([]byte(h + "." + b64u(c)))
			return h + "." + b64u(c) + "." + b64u(m.Sum(nil)), 200, ""
		}, "not accepted"},
		"not a token": {func(string) (string, int, string) { return "abc", 200, "" }, "not a signed token"},
		"the provider refuses": {func(string) (string, int, string) {
			return "", 400, `{"error":"invalid_grant","error_description":"code was used"}`
		}, "invalid_grant: code was used"},
		"no id token": {func(string) (string, int, string) { return "", 200, `{"access_token":"x"}` }, "did not give a token"},
	}
	for name, c := range cases {
		e.p.mint = c.mint
		if _, err := e.ok(t); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v (wanted %q)", name, err, c.want)
		}
	}
	// the start itself: what the browser brings back has to belong to this sign-in
	e.p.mint = func(n string) (string, int, string) {
		return e.p.tokenRS256("k1", e.p.rsaKeys["k1"], e.p.claims(n, e.now)), 200, ""
	}
	ck, q := e.begin(t)
	good := url.Values{"code": {"code-" + q.Get("state")}, "state": {q.Get("state")}}
	for name, c := range map[string]struct {
		ck   *http.Cookie
		q    url.Values
		want string
	}{
		"no cookie (another browser)": {nil, good, "not started in this browser"},
		"another state":               {ck, url.Values{"code": {"x"}, "state": {"forged"}}, "state"},
		"no state":                    {ck, url.Values{"code": {"x"}}, "state"},
		"no code":                     {ck, url.Values{"state": {q.Get("state")}}, "no code"},
		"a refusal by the provider":   {ck, url.Values{"error": {"access_denied"}, "error_description": {"nope"}}, "access_denied: nope"},
		"a changed cookie":            {&http.Cookie{Name: CookieName, Value: ck.Value[:len(ck.Value)-3] + "AAA"}, good, "expired"},
		"a cookie that is not signed": {&http.Cookie{Name: CookieName, Value: b64u([]byte(`{"s":"forged","e":9999999999}`)) + "." + b64u([]byte("x"))}, good, "expired"},
	} {
		if _, err := e.finish(c.ck, c.q); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v (wanted %q)", name, err, c.want)
		}
	}
	e.now = e.now.Add(11 * time.Minute) // a sign-in that took too long
	if _, err := e.finish(ck, good); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("an old sign-in: %v", err)
	}
}

func TestKeysThatAreRotated(t *testing.T) {
	e := newEnv(t)
	if _, err := e.ok(t); err != nil {
		t.Fatal(err)
	}
	if e.p.jwksFetches != 1 {
		t.Fatalf("the keys are read once and kept: %d", e.p.jwksFetches)
	}
	if _, err := e.ok(t); err != nil || e.p.jwksFetches != 1 {
		t.Fatalf("%v %d", err, e.p.jwksFetches)
	}
	// the provider changes its key
	k2, _ := rsa.GenerateKey(rand.Reader, 2048)
	e.p.mu.Lock()
	e.p.rsaKeys["k2"], e.p.listed = k2, []string{"k2"}
	e.p.mu.Unlock()
	e.p.mint = func(n string) (string, int, string) { return e.p.tokenRS256("k2", k2, e.p.claims(n, e.now)), 200, "" }
	e.now = e.now.Add(2 * time.Minute)
	if _, err := e.ok(t); err != nil || e.p.jwksFetches != 2 {
		t.Fatalf("an unknown key makes the list be read again: %v %d", err, e.p.jwksFetches)
	}
	// and an unknown key does not make Lumen read the list over and over
	e.p.mint = func(n string) (string, int, string) { return e.p.tokenRS256("k9", k2, e.p.claims(n, e.now)), 200, "" }
	e.now = e.now.Add(2 * time.Minute)
	e.ok(t)
	before := e.p.jwksFetches
	for i := 0; i < 5; i++ {
		e.ok(t)
	}
	if e.p.jwksFetches != before {
		t.Fatalf("at most once a minute: %d -> %d", before, e.p.jwksFetches)
	}
}

func TestWhatTheProviderSaysAboutItselfIsChecked(t *testing.T) {
	e := newEnv(t)
	e.p.issuerSay = "https://another.example.com"
	e.svc.disc = nil
	if _, _, err := func() (int, int, error) { _, err := e.svc.Discover(context.Background(), e.cfg); return 0, 0, err }(); err == nil || !strings.Contains(err.Error(), "the issuer must be written exactly") {
		t.Fatalf("a provider that names another issuer: %v", err)
	}
	e.p.issuerSay = ""
	if _, err := e.svc.Discover(context.Background(), e.cfg); err != nil {
		t.Fatal(err)
	}
	// addresses: https, or a provider on this machine; plain http elsewhere only when it is allowed
	s := &Service{}
	for raw, ok := range map[string]bool{"https://idp.example.com/realms/x": true, "http://localhost:8080": true, "http://127.0.0.1:9000": true, "http://idp.example.com": false, "ftp://idp.example.com": false, "https://user:pw@idp.example.com": false, "idp.example.com": false, "https://": false} {
		if err := s.CheckURL(raw); (err == nil) != ok {
			t.Errorf("%q: %v", raw, err)
		}
	}
	s.AllowInsecure = true
	if err := s.CheckURL("http://keycloak.lan:8080"); err != nil {
		t.Fatalf("allowed for a closed network: %v", err)
	}
	if (&Service{PublicURL: ""}).RedirectURI() != "" || (&Service{PublicURL: "https://x.example.com/"}).RedirectURI() != "https://x.example.com/auth/oidc/callback" {
		t.Fatal("redirect uri")
	}
	if _, err := (&Service{PublicURL: ""}).Begin(context.Background(), httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), e.cfg); err == nil || !strings.Contains(err.Error(), "LUMEN_PUBLIC_URL") {
		t.Fatalf("without a public address: %v", err)
	}
}

func TestEmailVerifiedInItsForms(t *testing.T) {
	e := newEnv(t)
	for name, c := range map[string]struct {
		v    any
		want string
	}{"true": {true, "true"}, "false": {false, "false"}, "the string true": {"true", "true"}, "the string false": {"false", "false"}, "not said": {nil, "nil"}} {
		e.p.mint = func(n string) (string, int, string) {
			cl := e.p.claims(n, e.now)
			if c.v == nil {
				delete(cl, "email_verified")
			} else {
				cl["email_verified"] = c.v
			}
			return e.p.tokenRS256("k1", e.p.rsaKeys["k1"], cl), 200, ""
		}
		id, err := e.ok(t)
		got := "nil"
		if id.EmailVerified != nil {
			got = fmt.Sprint(*id.EmailVerified)
		}
		if err != nil || got != c.want {
			t.Errorf("%s: %s %v", name, got, err)
		}
	}
}

func TestTheSecretIsKeptSealed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	d, _ := e.db.Get(ctx, metaColl, metaID)
	if strings.Contains(string(d.Data), "the-client-secret") || e.cfg.Secret == "" || e.svc.clientSecret(e.cfg) != "the-client-secret" {
		t.Fatalf("sealed in the store, readable by Lumen: %s", d.Data)
	}
	c := e.cfg
	c.Label = "Company login"
	e.svc.Save(ctx, c, "") // no new secret: the old one stays
	if got, _ := e.svc.Load(ctx); e.svc.clientSecret(got) != "the-client-secret" || got.Label != "Company login" {
		t.Fatalf("%+v", got)
	}
	e.svc.Save(ctx, c, "a-new-secret")
	if got, _ := e.svc.Load(ctx); e.svc.clientSecret(got) != "a-new-secret" {
		t.Fatal("replaced")
	}
}
