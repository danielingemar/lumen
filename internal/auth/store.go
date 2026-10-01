package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
)

var (
	userRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9._@-]{0,63}$`)
	tenantRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	dummyOnce sync.Once
	dummyHash string // verified against for unknown users, so timing does not reveal which usernames exist
)

func dummy() string {
	dummyOnce.Do(func() { dummyHash, _ = HashPassword("dummy-password-for-timing") })
	return dummyHash
}

type User struct {
	Name    string    `json:"name"`
	Tenant  string    `json:"tenant"`
	Hash    string    `json:"hash,omitempty"`
	Group   string    `json:"group,omitempty"` // empty = admin (accounts from before groups existed)
	Created time.Time `json:"created"`
}

// APIKey is stored only as a SHA-256 hash (which is also its document id); the plain key is shown
// once at creation. ID is a short non-secret handle for listing and deleting.
type APIKey struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Tenant  string    `json:"tenant"`
	Prefix  string    `json:"prefix"` // first characters, to recognise a key in a list
	Hash    string    `json:"hash,omitempty"`
	Created time.Time `json:"created"`
}

const (
	collUsers = "users"
	collKeys  = "keys"
	collMeta  = "meta"
)

// Store keeps users, API keys and the session secret in a document backend (Elasticsearch, or a JSON file).
type Store struct {
	b      docstore.Backend
	ttl    time.Duration // how long reads may be cached; 0 disables caching (file backend, tests)
	mu     sync.Mutex
	secret []byte
	users  map[string]cachedUser
	keys   map[string]cachedKey
	groups map[string]cachedGroup
}

type cachedGroup struct {
	g   Group
	ok  bool
	exp time.Time
}

type cachedUser struct {
	u   User
	ok  bool
	exp time.Time
}
type cachedKey struct {
	k   APIKey
	exp time.Time
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// Open uses any backend. cacheTTL > 0 caches user and key lookups briefly (useful when every request
// would otherwise hit Elasticsearch); local writes always invalidate the cache.
func Open(b docstore.Backend, cacheTTL time.Duration) (*Store, error) {
	s := &Store{b: b, ttl: cacheTTL, users: map[string]cachedUser{}, keys: map[string]cachedKey{}, groups: map[string]cachedGroup{}}
	if _, err := s.loadSecret(); err != nil {
		return nil, err
	}
	return s, nil
}

// OpenDir is the single-node default: a JSON document file in dir.
func OpenDir(dir string) (*Store, error) {
	f, err := docstore.OpenFile(dir)
	if err != nil {
		return nil, err
	}
	return Open(f, 0)
}

func (s *Store) Backend() docstore.Backend { return s.b }

func (s *Store) invalidate() {
	s.mu.Lock()
	s.users, s.keys, s.groups = map[string]cachedUser{}, map[string]cachedKey{}, map[string]cachedGroup{}
	s.mu.Unlock()
}

func (s *Store) loadSecret() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secret != nil {
		return s.secret, nil
	}
	c, cancel := ctx()
	defer cancel()
	d, err := s.b.Get(c, collMeta, "session_secret")
	if errors.Is(err, docstore.ErrNotFound) {
		err = s.b.Create(c, collMeta, "session_secret", map[string]string{"value": RandomToken(32)})
		if err == nil || errors.Is(err, docstore.ErrExists) { // lost a race with another instance: use theirs
			d, err = s.b.Get(c, collMeta, "session_secret")
		}
	}
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := d.Decode(&m); err != nil || m["value"] == "" {
		return nil, errors.New("session secret is corrupt")
	}
	s.secret = []byte(m["value"])
	return s.secret, nil
}

// Secret is the HMAC key for session cookies.
func (s *Store) Secret() []byte { b, _ := s.loadSecret(); return b }

func (s *Store) HasCredentials() bool {
	c, cancel := ctx()
	defer cancel()
	u, _ := s.b.List(c, collUsers, nil, 1)
	k, _ := s.b.List(c, collKeys, nil, 1)
	return len(u)+len(k) > 0
}

func NormalizeUser(name string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	if !userRe.MatchString(n) {
		return "", errors.New("username must be 1-64 characters: letters, digits, . _ @ -")
	}
	return n, nil
}

// CreateUser creates an admin (the default before groups existed; used for the bootstrap account and tests).
func (s *Store) CreateUser(name, tenant, password string) error {
	return s.CreateUserIn(name, tenant, password, "admin")
}

// CreateUserIn creates a user in a built-in or custom group of the tenant.
func (s *Store) CreateUserIn(name, tenant, password, group string) error {
	if _, ok := s.GetGroup(tenant, group); !ok {
		return errors.New("no such group")
	}
	n, err := NormalizeUser(name)
	if err != nil {
		return err
	}
	if !tenantRe.MatchString(tenant) {
		return errors.New("tenant must be letters, digits, _ or -")
	}
	h, err := HashPassword(password)
	if err != nil {
		return err
	}
	c, cancel := ctx()
	defer cancel()
	err = s.b.Create(c, collUsers, n, User{Name: n, Tenant: tenant, Hash: h, Group: group, Created: time.Now().UTC()})
	if errors.Is(err, docstore.ErrExists) {
		return errors.New("user already exists")
	}
	s.invalidate()
	return err
}

func (s *Store) SetPassword(name, password string) error {
	n, err := NormalizeUser(name)
	if err != nil {
		return err
	}
	h, err := HashPassword(password)
	if err != nil {
		return err
	}
	c, cancel := ctx()
	defer cancel()
	d, err := s.b.Get(c, collUsers, n)
	if errors.Is(err, docstore.ErrNotFound) {
		return errors.New("no such user")
	}
	if err != nil {
		return err
	}
	var u User
	if err := d.Decode(&u); err != nil {
		return err
	}
	u.Hash = h
	err = s.b.Put(c, collUsers, n, u, d.Version)
	s.invalidate()
	return err
}

// SetGroup moves a user to another group of the same tenant.
func (s *Store) SetGroup(name, group string) error {
	n, err := NormalizeUser(name)
	if err != nil {
		return err
	}
	c, cancel := ctx()
	defer cancel()
	d, err := s.b.Get(c, collUsers, n)
	if err != nil {
		return errors.New("no such user")
	}
	var u User
	if err := d.Decode(&u); err != nil {
		return err
	}
	if _, ok := s.GetGroup(u.Tenant, group); !ok {
		return errors.New("no such group")
	}
	u.Group = group
	err = s.b.Put(c, collUsers, n, u, d.Version)
	s.invalidate()
	return err
}

// ListUsersIn lists one tenant's users (without password hashes).
func (s *Store) ListUsersIn(tenant string) []User {
	c, cancel := ctx()
	defer cancel()
	docs, _ := s.b.List(c, collUsers, map[string]string{"tenant": tenant}, 1000)
	out := []User{}
	for _, d := range docs {
		var u User
		if d.Decode(&u) == nil {
			u.Hash = ""
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) DeleteUser(name string) error {
	n, err := NormalizeUser(name)
	if err != nil {
		return err
	}
	c, cancel := ctx()
	defer cancel()
	if _, err := s.b.Get(c, collUsers, n); errors.Is(err, docstore.ErrNotFound) {
		return errors.New("no such user")
	}
	err = s.b.Delete(c, collUsers, n)
	s.invalidate()
	return err
}

func (s *Store) GetUser(name string) (User, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	if s.ttl > 0 {
		s.mu.Lock()
		if e, ok := s.users[n]; ok && time.Now().Before(e.exp) {
			s.mu.Unlock()
			return e.u, e.ok
		}
		s.mu.Unlock()
	}
	c, cancel := ctx()
	defer cancel()
	var u User
	d, err := s.b.Get(c, collUsers, n)
	ok := err == nil && d.Decode(&u) == nil
	if err != nil && !errors.Is(err, docstore.ErrNotFound) {
		return User{}, false // backend trouble: fail closed, do not cache
	}
	if s.ttl > 0 {
		s.mu.Lock()
		s.users[n] = cachedUser{u, ok, time.Now().Add(s.ttl)}
		s.mu.Unlock()
	}
	return u, ok
}

func (s *Store) ListUsers() []User {
	c, cancel := ctx()
	defer cancel()
	docs, _ := s.b.List(c, collUsers, nil, 1000)
	out := []User{}
	for _, d := range docs {
		var u User
		if d.Decode(&u) == nil {
			u.Hash = ""
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// VerifyLogin checks a username and password. It always does one full password hash, even for
// unknown users, so response time does not reveal which usernames exist.
func (s *Store) VerifyLogin(name, password string) (User, bool) {
	u, ok := s.GetUser(strings.TrimSpace(name))
	h := u.Hash
	if !ok {
		h = dummy()
	}
	good := VerifyPassword(password, h)
	return u, ok && good
}

// ---- API keys ----

func hashKey(k string) string { h := sha256.Sum256([]byte(k)); return hex.EncodeToString(h[:]) }

// CreateKey makes a new agent key for a tenant and returns the plain key. It cannot be recovered later.
func (s *Store) CreateKey(tenant, name string) (string, APIKey, error) {
	if !tenantRe.MatchString(tenant) {
		return "", APIKey{}, errors.New("invalid tenant")
	}
	name = strings.TrimSpace(name)
	if len(name) > 80 {
		name = name[:80]
	}
	plain := "lmn_" + RandomToken(24)
	h := hashKey(plain)
	k := APIKey{ID: h[:12], Name: name, Tenant: tenant, Prefix: plain[:8], Hash: h, Created: time.Now().UTC()}
	c, cancel := ctx()
	defer cancel()
	if err := s.b.Create(c, collKeys, h, k); err != nil {
		return "", APIKey{}, err
	}
	s.invalidate()
	k.Hash = ""
	return plain, k, nil
}

// KeyByHash looks a key up by the hash of its plain text.
func (s *Store) KeyByHash(h string) (APIKey, bool) {
	if s.ttl > 0 {
		s.mu.Lock()
		if e, ok := s.keys[h]; ok && time.Now().Before(e.exp) {
			s.mu.Unlock()
			return e.k, true
		}
		s.mu.Unlock()
	}
	c, cancel := ctx()
	defer cancel()
	d, err := s.b.Get(c, collKeys, h)
	var k APIKey
	if err != nil || d.Decode(&k) != nil {
		return APIKey{}, false // unknown keys are not cached, so a new key works immediately
	}
	if s.ttl > 0 {
		s.mu.Lock()
		s.keys[h] = cachedKey{k, time.Now().Add(s.ttl)}
		s.mu.Unlock()
	}
	return k, true
}

// LookupKey returns the tenant for a presented key.
func (s *Store) LookupKey(plain string) (string, bool) {
	k, ok := s.KeyByHash(hashKey(plain))
	return k.Tenant, ok
}

func (s *Store) ListKeys(tenant string) []APIKey {
	c, cancel := ctx()
	defer cancel()
	docs, _ := s.b.List(c, collKeys, map[string]string{"tenant": tenant}, 1000)
	out := []APIKey{}
	for _, d := range docs {
		var k APIKey
		if d.Decode(&k) == nil {
			k.Hash = ""
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func (s *Store) DeleteKey(tenant, id string) error {
	c, cancel := ctx()
	defer cancel()
	docs, err := s.b.List(c, collKeys, map[string]string{"tenant": tenant, "id": id}, 2)
	if err != nil {
		return err
	}
	if len(docs) != 1 {
		return errors.New("no such key")
	}
	err = s.b.Delete(c, collKeys, docs[0].ID)
	s.invalidate()
	return err
}

// ImportLegacy moves users and keys from the pre-Elasticsearch auth.json (if present) into the store,
// then renames the file so it is not imported twice. Returns the number of records imported.
func ImportLegacy(s *Store, dir string) (int, error) {
	path := filepath.Join(dir, "auth.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, nil
	}
	var old struct {
		Secret string            `json:"session_secret"`
		Users  map[string]User   `json:"users"`
		Keys   map[string]APIKey `json:"keys"`
	}
	if err := json.Unmarshal(b, &old); err != nil {
		return 0, err
	}
	c, cancel := ctx()
	defer cancel()
	n := 0
	for name, u := range old.Users {
		if err := s.b.Create(c, collUsers, name, u); err == nil {
			n++
		}
	}
	for _, k := range old.Keys {
		if k.Hash == "" {
			continue
		}
		if k.ID == "" || len(k.Hash) < 12 {
			k.ID = k.Hash[:min(12, len(k.Hash))]
		}
		kk := k
		kk.ID = k.Hash[:12]
		if err := s.b.Create(c, collKeys, k.Hash, kk); err == nil {
			n++
		}
	}
	s.invalidate()
	return n, os.Rename(path, path+".migrated")
}
