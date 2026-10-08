// Package oidc lets people sign in with an OpenID Connect provider (Keycloak, Authentik, Entra ID, Google and so on): the
// authorization code flow with PKCE, and a strict check of the ID token. It uses nothing but the standard library.
//
// Deliberately narrow: one provider, RS256 and ES256 signatures only (never "none", never a shared-secret algorithm), and the
// token is checked for signature, issuer, audience, expiry and the nonce of this very sign-in before anything is believed.
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
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/secretbox"
)

const (
	metaColl = "meta"
	metaID   = "oidc"
	// CollLinks holds which provider identity belongs to which local user.
	CollLinks = "oidc_links"

	CookieName = "lumen_oidc"
	skew       = 60 * time.Second
	flowTTL    = 10 * time.Minute
)

// Config is the one provider that is set up. The secret is kept sealed.
type Config struct {
	Enabled      bool     `json:"enabled"`
	Issuer       string   `json:"issuer"`
	ClientID     string   `json:"client_id"`
	Secret       string   `json:"secret"` // sealed
	Label        string   `json:"label"`  // on the button: "Sign in with …"
	Domains      []string `json:"domains"`
	AutoCreate   bool     `json:"auto_create"`
	DefaultGroup string   `json:"default_group"`
	LinkByEmail  bool     `json:"link_by_email"`
	Tenant       string   `json:"tenant"` // the tenant people who sign in belong to
	UpdatedBy    string   `json:"updated_by"`
}

// Identity is who the provider says signed in.
type Identity struct {
	Subject       string
	Email         string
	EmailVerified *bool // nil: the provider did not say
	Name          string
	Username      string // preferred_username, if any
}

// Service does the work. The zero value is not usable: fill DB, Box and Secret.
type Service struct {
	DB        docstore.Backend
	Box       *secretbox.Box
	PublicURL string        // for the address the provider sends people back to
	Secret    func() []byte // signs the cookie of a sign-in that is under way
	HTTP      *http.Client
	Now       func() time.Time
	// AllowInsecure lets an issuer be reached over plain http (a provider inside a closed network). Without it only https
	// and a provider on this very machine are accepted.
	AllowInsecure bool

	mu       sync.Mutex
	disc     *discovery
	discFor  string
	discAt   time.Time
	keys     map[string]crypto.PublicKey
	keysAt   time.Time
	keysTry  time.Time
	keysFrom string
}

// Discovery is what a provider says about itself.
type Discovery = discovery

type discovery struct {
	Issuer    string   `json:"issuer"`
	AuthURL   string   `json:"authorization_endpoint"`
	TokenURL  string   `json:"token_endpoint"`
	JWKSURL   string   `json:"jwks_uri"`
	AuthModes []string `json:"token_endpoint_auth_methods_supported"`
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return errors.New("too many redirects")
		}
		return nil
	}}
}

// ---- the setting ----

// Load reads the setting.
func (s *Service) Load(ctx context.Context) (Config, bool) {
	d, err := s.DB.Get(ctx, metaColl, metaID)
	if err != nil {
		return Config{}, false
	}
	var c Config
	if d.Decode(&c) != nil {
		return Config{}, false
	}
	return c, true
}

// Save stores the setting. An empty newSecret keeps the one that is there.
func (s *Service) Save(ctx context.Context, c Config, newSecret string) error {
	if newSecret != "" {
		c.Secret = s.Box.Seal(newSecret)
	} else if old, ok := s.Load(ctx); ok {
		c.Secret = old.Secret
	}
	return s.DB.Put(ctx, metaColl, metaID, c, "")
}

func (s *Service) clientSecret(c Config) string {
	if c.Secret == "" {
		return ""
	}
	v, _ := s.Box.Open(c.Secret)
	return v
}

// RedirectURI is the address the provider sends people back to. It has to be registered at the provider.
func (s *Service) RedirectURI() string {
	if s.PublicURL == "" {
		return ""
	}
	return strings.TrimRight(s.PublicURL, "/") + "/auth/oidc/callback"
}

// CheckURL says whether an address of the provider may be used.
func (s *Service) CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("%q is not an address", raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		h := u.Hostname()
		if s.AllowInsecure || h == "localhost" || h == "127.0.0.1" || h == "::1" {
			return nil
		}
		return fmt.Errorf("%s is not https. A provider is reached over https; (for one inside a closed network, set LUMEN_OIDC_ALLOW_HTTP=1)", raw)
	}
	return fmt.Errorf("%q must start with https://", raw)
}

func (s *Service) getJSON(ctx context.Context, raw string, into any) error {
	if err := s.CheckURL(raw); err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", raw, nil)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client().Do(req)
	if err != nil {
		return reason(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s answered HTTP %d", raw, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(into)
}

func reason(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// Discover reads what the provider says about itself, and checks it. The issuer it names must be the one that was set up:
// otherwise a provider could pass off another's tokens.
func (s *Service) Discover(ctx context.Context, c Config) (*discovery, error) {
	s.mu.Lock()
	if s.disc != nil && s.discFor == c.Issuer && s.now().Sub(s.discAt) < 10*time.Minute {
		d := s.disc
		s.mu.Unlock()
		return d, nil
	}
	s.mu.Unlock()
	if c.Issuer == "" {
		return nil, errors.New("no issuer is set")
	}
	var d discovery
	if err := s.getJSON(ctx, strings.TrimRight(c.Issuer, "/")+"/.well-known/openid-configuration", &d); err != nil {
		return nil, fmt.Errorf("the provider could not be asked: %w", err)
	}
	if strings.TrimRight(d.Issuer, "/") != strings.TrimRight(c.Issuer, "/") {
		return nil, fmt.Errorf("the provider says it is %q, not %q: the issuer must be written exactly as the provider does", d.Issuer, c.Issuer)
	}
	for name, v := range map[string]string{"authorization_endpoint": d.AuthURL, "token_endpoint": d.TokenURL, "jwks_uri": d.JWKSURL} {
		if v == "" {
			return nil, fmt.Errorf("the provider does not give a %s", name)
		}
		if err := s.CheckURL(v); err != nil {
			return nil, fmt.Errorf("the %s of the provider: %w", name, err)
		}
	}
	s.mu.Lock()
	s.disc, s.discFor, s.discAt = &d, c.Issuer, s.now()
	s.mu.Unlock()
	return &d, nil
}

// ---- the start of a sign-in ----

type flow struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Exp      int64  `json:"e"`
}

func rnd(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Service) sign(payload []byte) string {
	m := hmac.New(sha256.New, s.Secret())
	m.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Service) unsign(v string) ([]byte, bool) {
	p := strings.SplitN(v, ".", 2)
	if len(p) != 2 {
		return nil, false
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(p[0])
	mac, err2 := base64.RawURLEncoding.DecodeString(p[1])
	if err1 != nil || err2 != nil {
		return nil, false
	}
	m := hmac.New(sha256.New, s.Secret())
	m.Write(payload)
	return payload, hmac.Equal(m.Sum(nil), mac)
}

// Begin starts a sign-in: it remembers state, nonce and the PKCE secret in a signed cookie that only this browser has, and
// returns where to send the browser.
func (s *Service) Begin(ctx context.Context, w http.ResponseWriter, r *http.Request, c Config) (string, error) {
	if s.RedirectURI() == "" {
		return "", errors.New("LUMEN_PUBLIC_URL is not set, so Lumen does not know the address the provider sends people back to")
	}
	d, err := s.Discover(ctx, c)
	if err != nil {
		return "", err
	}
	f := flow{State: rnd(18), Nonce: rnd(18), Verifier: rnd(32), Exp: s.now().Add(flowTTL).Unix()}
	b, _ := json.Marshal(f)
	secure := strings.HasPrefix(s.PublicURL, "https://") || r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: s.sign(b), Path: "/auth/oidc", MaxAge: int(flowTTL.Seconds()), HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
	chal := sha256.Sum256([]byte(f.Verifier))
	u, _ := url.Parse(d.AuthURL)
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", c.ClientID)
	q.Set("redirect_uri", s.RedirectURI())
	q.Set("scope", "openid email profile")
	q.Set("state", f.State)
	q.Set("nonce", f.Nonce)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(chal[:]))
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ---- the end of a sign-in ----

// Finish handles the browser coming back: it checks that this sign-in was started here, trades the code for a token, and
// checks the token. The cookie is used up either way.
func (s *Service) Finish(ctx context.Context, w http.ResponseWriter, r *http.Request, c Config) (Identity, error) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/auth/oidc", MaxAge: -1, HttpOnly: true})
	if e := r.URL.Query().Get("error"); e != "" {
		if d := r.URL.Query().Get("error_description"); d != "" {
			e += ": " + d
		}
		return Identity{}, fmt.Errorf("the provider refused the sign-in (%s)", clip(e, 160))
	}
	ck, err := r.Cookie(CookieName)
	if err != nil {
		return Identity{}, errors.New("this sign-in was not started in this browser, or it took too long. Start again")
	}
	raw, ok := s.unsign(ck.Value)
	var f flow
	if !ok || json.Unmarshal(raw, &f) != nil || s.now().Unix() > f.Exp {
		return Identity{}, errors.New("this sign-in has expired. Start again")
	}
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	if subtle.ConstantTimeCompare([]byte(state), []byte(f.State)) != 1 || state == "" {
		return Identity{}, errors.New("the answer does not belong to this sign-in (state). Start again")
	}
	if code == "" {
		return Identity{}, errors.New("the provider gave no code")
	}
	d, err := s.Discover(ctx, c)
	if err != nil {
		return Identity{}, err
	}
	idt, err := s.exchange(ctx, c, d, code, f.Verifier)
	if err != nil {
		return Identity{}, err
	}
	return s.verify(ctx, c, d, idt, f.Nonce)
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func (s *Service) exchange(ctx context.Context, c Config, d *discovery, code, verifier string) (string, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {s.RedirectURI()}, "code_verifier": {verifier}}
	secret := s.clientSecret(c)
	post := false
	if len(d.AuthModes) > 0 { // use what the provider supports; the standard default is basic
		post = true
		for _, m := range d.AuthModes {
			if m == "client_secret_basic" {
				post = false
			}
		}
	}
	if post || secret == "" {
		form.Set("client_id", c.ClientID)
		if secret != "" {
			form.Set("client_secret", secret)
		}
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", d.TokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if !post && secret != "" {
		req.SetBasicAuth(url.QueryEscape(c.ClientID), url.QueryEscape(secret))
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("the provider could not be reached to finish the sign-in: %w", reason(err))
	}
	defer resp.Body.Close()
	var out struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
		Desc    string `json:"error_description"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if resp.StatusCode != 200 || out.IDToken == "" {
		msg := out.Error
		if out.Desc != "" {
			msg += ": " + out.Desc
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return "", fmt.Errorf("the provider did not give a token (%s)", clip(msg, 160))
	}
	return out.IDToken, nil
}

// ---- the token ----

type claims struct {
	Iss           string          `json:"iss"`
	Sub           string          `json:"sub"`
	Aud           json.RawMessage `json:"aud"`
	Exp           float64         `json:"exp"`
	Iat           float64         `json:"iat"`
	Nbf           float64         `json:"nbf"`
	Nonce         string          `json:"nonce"`
	Azp           string          `json:"azp"`
	Email         string          `json:"email"`
	EmailVerified json.RawMessage `json:"email_verified"`
	Name          string          `json:"name"`
	Username      string          `json:"preferred_username"`
}

func audiences(raw json.RawMessage) []string {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(raw, &many)
	return many
}

func (s *Service) verify(ctx context.Context, c Config, d *discovery, token, nonce string) (Identity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, errors.New("the token is not a signed token")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	hb, e1 := base64.RawURLEncoding.DecodeString(parts[0])
	pb, e2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, e3 := base64.RawURLEncoding.DecodeString(parts[2])
	if e1 != nil || e2 != nil || e3 != nil || json.Unmarshal(hb, &hdr) != nil {
		return Identity{}, errors.New("the token cannot be read")
	}
	if hdr.Alg != "RS256" && hdr.Alg != "ES256" { // never "none", never a shared-secret algorithm
		return Identity{}, fmt.Errorf("the token is signed with %q, which is not accepted (RS256 and ES256 are)", hdr.Alg)
	}
	key, err := s.key(ctx, d, hdr.Kid, hdr.Alg)
	if err != nil {
		return Identity{}, err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	switch k := key.(type) {
	case *rsa.PublicKey:
		if hdr.Alg != "RS256" || rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], sig) != nil {
			return Identity{}, errors.New("the signature of the token is not valid")
		}
	case *ecdsa.PublicKey:
		if hdr.Alg != "ES256" || len(sig) != 64 || !ecdsa.Verify(k, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
			return Identity{}, errors.New("the signature of the token is not valid")
		}
	default:
		return Identity{}, errors.New("the key of the token is not usable")
	}
	var cl claims
	if json.Unmarshal(pb, &cl) != nil {
		return Identity{}, errors.New("the token cannot be read")
	}
	now := s.now()
	switch {
	case strings.TrimRight(cl.Iss, "/") != strings.TrimRight(c.Issuer, "/"):
		return Identity{}, fmt.Errorf("the token comes from %q, not from %q", cl.Iss, c.Issuer)
	case !contains(audiences(cl.Aud), c.ClientID):
		return Identity{}, errors.New("the token is for another application (audience)")
	case len(audiences(cl.Aud)) > 1 && cl.Azp != c.ClientID:
		return Identity{}, errors.New("the token was given to another application (azp)")
	case cl.Exp == 0 || now.After(time.Unix(int64(cl.Exp), 0).Add(skew)):
		return Identity{}, errors.New("the token has expired")
	case cl.Iat != 0 && time.Unix(int64(cl.Iat), 0).After(now.Add(skew)):
		return Identity{}, errors.New("the token is from the future: the clocks of Lumen and the provider differ")
	case cl.Nbf != 0 && time.Unix(int64(cl.Nbf), 0).After(now.Add(skew)):
		return Identity{}, errors.New("the token is not valid yet")
	case cl.Nonce == "" || subtle.ConstantTimeCompare([]byte(cl.Nonce), []byte(nonce)) != 1:
		return Identity{}, errors.New("the token was not made for this sign-in (nonce)")
	case cl.Sub == "":
		return Identity{}, errors.New("the token does not say who signed in")
	}
	id := Identity{Subject: cl.Sub, Email: strings.ToLower(strings.TrimSpace(cl.Email)), Name: cl.Name, Username: cl.Username}
	if len(cl.EmailVerified) > 0 {
		var b bool
		var str string
		if json.Unmarshal(cl.EmailVerified, &b) == nil {
			id.EmailVerified = &b
		} else if json.Unmarshal(cl.EmailVerified, &str) == nil {
			b = strings.EqualFold(str, "true")
			id.EmailVerified = &b
		}
	}
	return id, nil
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// key finds the public key that signed a token. An unknown key makes the provider's list be fetched again (keys are rotated),
// at most once a minute.
func (s *Service) key(ctx context.Context, d *discovery, kid, alg string) (crypto.PublicKey, error) {
	s.mu.Lock()
	k, ok := s.keys[kid]
	fresh := s.keysFrom == d.JWKSURL && s.now().Sub(s.keysAt) < time.Hour
	canTry := s.now().Sub(s.keysTry) > time.Minute
	s.mu.Unlock()
	if ok && fresh {
		return k, nil
	}
	if !canTry && !ok {
		return nil, errors.New("the token is signed with a key the provider does not list (it was looked for a moment ago)")
	}
	s.mu.Lock()
	s.keysTry = s.now()
	s.mu.Unlock()
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := s.getJSON(ctx, d.JWKSURL, &set); err != nil {
		return nil, fmt.Errorf("the keys of the provider could not be read: %w", err)
	}
	keys := map[string]crypto.PublicKey{}
	for _, j := range set.Keys {
		if j.Use != "" && j.Use != "sig" {
			continue
		}
		if pk := j.public(); pk != nil {
			keys[j.Kid] = pk
		}
	}
	s.mu.Lock()
	s.keys, s.keysAt, s.keysFrom = keys, s.now(), d.JWKSURL
	s.mu.Unlock()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	if kid == "" && len(keys) == 1 { // a provider with one key may leave the id out
		for _, k := range keys {
			return k, nil
		}
	}
	return nil, errors.New("the token is signed with a key the provider does not list")
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func b64(v string) *big.Int {
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return nil
	}
	return new(big.Int).SetBytes(b)
}

func (j jwk) public() crypto.PublicKey {
	switch j.Kty {
	case "RSA":
		n, e := b64(j.N), b64(j.E)
		if n == nil || e == nil || n.BitLen() < 2048 || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 {
			return nil
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}
	case "EC":
		x, y := b64(j.X), b64(j.Y)
		if j.Crv != "P-256" || x == nil || y == nil || !elliptic.P256().IsOnCurve(x, y) {
			return nil
		}
		return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
	}
	return nil
}

var _ = net.IPv4len

// Forget throws away what was learned about the provider (after the setting changed).
func (s *Service) Forget() {
	s.mu.Lock()
	s.disc, s.keys, s.keysAt, s.keysTry = nil, nil, time.Time{}, time.Time{}
	s.mu.Unlock()
}
