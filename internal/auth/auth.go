package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/perm"
)

const CookieName = "lumen_session"

// SessionTTL is how long a browser login stays valid.
const SessionTTL = 12 * time.Hour

type payload struct {
	U string `json:"u,omitempty"`
	T string `json:"t"`
	V string `json:"v,omitempty"` // fingerprint of the password hash: changing the password ends old sessions
	K string `json:"k,omitempty"` // for API-key logins: hash of the key (or "env:<hash>"); the session ends if the key is deleted
	E int64  `json:"e"`
}

func fingerprint(hash string) string {
	h := sha256.Sum256([]byte(hash))
	return hex.EncodeToString(h[:6])
}

func sign(secret []byte, msg string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Auth authenticates requests from API keys (environment or stored) or browser sessions.
type Auth struct {
	Store   *Store
	EnvKeys map[string]string // bootstrap keys from LUMEN_API_KEYS: key -> tenant
	DevMode bool              // explicit LUMEN_DEV_MODE=true: allow unauthenticated access as tenant "default"
	Limiter *Limiter
}

func New(store *Store, envKeys map[string]string, dev bool) *Auth {
	return &Auth{Store: store, EnvKeys: envKeys, DevMode: dev, Limiter: NewLimiter(10, 10*time.Minute)}
}

func (a *Auth) IssueSession(u User) string {
	b, _ := json.Marshal(payload{U: u.Name, T: u.Tenant, V: fingerprint(u.Hash), E: time.Now().Add(SessionTTL).Unix()})
	msg := base64.RawURLEncoding.EncodeToString(b)
	return msg + "." + sign(a.Store.Secret(), msg)
}

func (a *Auth) verifyPayload(tok string) (payload, bool) {
	msg, sig, ok := strings.Cut(tok, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(sign(a.Store.Secret(), msg))) {
		return payload{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(msg)
	if err != nil {
		return payload{}, false
	}
	var p payload
	if json.Unmarshal(raw, &p) != nil || time.Now().Unix() > p.E {
		return payload{}, false
	}
	return p, true
}

func (a *Auth) envKeyTenant(hash string) (string, bool) {
	for k, t := range a.EnvKeys {
		if hmac.Equal([]byte(hashKey(k)), []byte(hash)) {
			return t, true
		}
	}
	return "", false
}

// parseSession validates a cookie and returns the identity behind it. User-password sessions end when the
// user is deleted, moved, or changes password; API-key sessions end when the key is deleted.
func (a *Auth) parseSession(tok string) (edition.Identity, bool) {
	p, ok := a.verifyPayload(tok)
	if !ok {
		return edition.Identity{}, false
	}
	if p.K != "" {
		if h, env := strings.CutPrefix(p.K, "env:"); env {
			t, ok := a.envKeyTenant(h)
			return edition.Identity{Tenant: t, Session: true, ViaKey: true, Perms: perm.Key()}, ok && t == p.T
		}
		k, ok := a.Store.KeyByHash(p.K)
		return edition.Identity{Tenant: k.Tenant, Session: true, ViaKey: true, Perms: perm.Key()}, ok && k.Tenant == p.T
	}
	u, ok := a.Store.GetUser(p.U)
	if !ok || u.Tenant != p.T || fingerprint(u.Hash) != p.V { // deleted user, moved tenant or changed password
		return edition.Identity{}, false
	}
	return edition.Identity{Tenant: u.Tenant, User: u.Name, Session: true, Perms: a.Store.PermsFor(u)}, true
}

// LoginWithKey exchanges a valid API key for a browser session (read access, dashboards; no key or password management).
func (a *Auth) LoginWithKey(plain string) (token, tenant string, ok bool) {
	var hash string
	if t, found := a.Store.LookupKey(plain); found {
		tenant, hash = t, hashKey(plain)
	} else if t, found := a.EnvKeys[plain]; found {
		tenant, hash = t, "env:"+hashKey(plain)
	} else {
		return "", "", false
	}
	b, _ := json.Marshal(payload{T: tenant, K: hash, E: time.Now().Add(SessionTTL).Unix()})
	msg := base64.RawURLEncoding.EncodeToString(b)
	return msg + "." + sign(a.Store.Secret(), msg), tenant, true
}

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return r.Header.Get("X-API-Key")
}

// Authenticate implements edition.Authenticator.
func (a *Auth) Authenticate(r *http.Request) (edition.Identity, error) {
	if key := bearer(r); key != "" {
		if t, ok := a.Store.LookupKey(key); ok {
			return edition.Identity{Tenant: t, Perms: perm.Key()}, nil
		}
		if len(a.EnvKeys) > 0 {
			if id, err := edition.NewAuthenticator(a.EnvKeys).Authenticate(r); err == nil {
				id.Perms = perm.Key()
				return id, nil
			}
		}
		return edition.Identity{}, edition.ErrUnauthorized
	}
	if c, err := r.Cookie(CookieName); err == nil {
		if id, ok := a.parseSession(c.Value); ok {
			return id, nil
		}
	}
	if a.DevMode {
		return edition.Identity{Tenant: "default", Perms: perm.Admin()}, nil
	}
	return edition.Identity{}, edition.ErrUnauthorized
}

// SetCookie writes the session cookie. Secure is set whenever the request arrived over HTTPS.
func SetCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode})
}

// Limiter blocks a username after too many failed logins in a time window.
type Limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	fails  map[string][]time.Time
}

func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, fails: map[string][]time.Time{}}
}

func (l *Limiter) prune(k string) []time.Time {
	cut := time.Now().Add(-l.window)
	var keep []time.Time
	for _, t := range l.fails[k] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(l.fails, k)
	} else {
		l.fails[k] = keep
	}
	return keep
}

func (l *Limiter) Blocked(k string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(k)) >= l.max
}
func (l *Limiter) Fail(k string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(k)
	l.fails[k] = append(l.fails[k], time.Now())
}
func (l *Limiter) Reset(k string) { l.mu.Lock(); delete(l.fails, k); l.mu.Unlock() }
