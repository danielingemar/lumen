// Package license checks Lumen Enterprise and Operator licences. A licence is a small signed file that is verified
// offline against public keys built into the program: Lumen never contacts anyone to check it (see docs/EDITIONS.md).
package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
)

const (
	Community  = "community"
	Enterprise = "enterprise"
	Operator   = "operator"

	// Grace is how long Enterprise features keep working after a licence has expired. Afterwards they stop; the
	// Community features and all data are not affected.
	Grace = 30 * 24 * time.Hour
	// ExpiringSoon is when the interface starts to remind about the expiry.
	ExpiringSoon = 30 * 24 * time.Hour

	formatName = "lumen-license-1"
	maxSize    = 16 << 10
)

// ErrInvalid wraps every reason a licence is refused, with a message that can be shown to a person.
var ErrInvalid = errors.New("invalid licence")

func bad(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(f, a...)) }

// Limits are soft: Lumen warns when they are reached or exceeded and never refuses data because of them. Zero = no limit.
type Limits struct {
	Hosts   int `json:"hosts,omitempty"`
	Tenants int `json:"tenants,omitempty"`
}

// Payload is what the signature covers.
type Payload struct {
	ID       string    `json:"id"`
	Customer string    `json:"customer"`
	Editions []string  `json:"editions"`
	Issued   time.Time `json:"issued"`
	Expires  time.Time `json:"expires"`
	Limits   Limits    `json:"limits"`
}

// File is the licence as stored on disk: the payload as signed (so nothing needs to be re-encoded to verify it),
// the signature, and the id of the key that made it.
type File struct {
	Format    string `json:"format"`
	KeyID     string `json:"key_id"`
	Payload   string `json:"payload"`   // base64 of the JSON of Payload
	Signature string `json:"signature"` // base64 of the Ed25519 signature
}

// License is a verified licence.
type License struct {
	Payload
	KeyID string
}

// Keys are the trusted public keys by id.
type Keys map[string]ed25519.PublicKey

func message(payload []byte) []byte { return append([]byte("LUMEN-LICENSE-V1\n"), payload...) }

var knownEditions = map[string]bool{Enterprise: true, Operator: true}

// Validate checks the content of a payload (used when issuing and when reading).
func (p Payload) Validate() error {
	if strings.TrimSpace(p.ID) == "" || len(p.ID) > 60 {
		return bad("the licence has no id")
	}
	if c := strings.TrimSpace(p.Customer); c == "" || len(c) > 120 || strings.ContainsAny(c, "\r\n\x00") {
		return bad("the customer name must be 1-120 characters on one line")
	}
	if len(p.Editions) == 0 {
		return bad("the licence names no edition")
	}
	for _, e := range p.Editions {
		if !knownEditions[e] {
			return bad("unknown edition %q (use %s or %s)", e, Enterprise, Operator)
		}
	}
	if p.Issued.IsZero() || p.Expires.IsZero() || !p.Expires.After(p.Issued) {
		return bad("the expiry date must be after the date of issue")
	}
	if p.Limits.Hosts < 0 || p.Limits.Tenants < 0 {
		return bad("limits cannot be negative")
	}
	return nil
}

// Sign makes a licence file. The private key never leaves the issuer.
func Sign(priv ed25519.PrivateKey, keyID string, p Payload) ([]byte, error) {
	if keyID == "" || strings.ContainsAny(keyID, " /\\\r\n") {
		return nil, bad("the key id must be a short name without spaces")
	}
	p.Issued, p.Expires = p.Issued.UTC().Truncate(time.Second), p.Expires.UTC().Truncate(time.Second)
	if err := p.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	f := File{Format: formatName, KeyID: keyID, Payload: base64.StdEncoding.EncodeToString(body), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, message(body)))}
	return json.MarshalIndent(f, "", "  ")
}

// Parse verifies a licence file against the trusted keys and returns it. It does not look at the clock.
func Parse(data []byte, keys Keys) (License, error) {
	if len(data) == 0 || len(data) > maxSize {
		return License{}, bad("that is not a Lumen licence file")
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil || f.Format != formatName || f.Payload == "" || f.Signature == "" {
		return License{}, bad("that is not a Lumen licence file (it should be the .license file you were sent, as it is)")
	}
	pub, ok := keys[f.KeyID]
	if !ok {
		if len(keys) == 0 {
			return License{}, bad("this build of Lumen trusts no licence keys, so it cannot verify licences; use the build you received with your licence")
		}
		return License{}, bad("the licence is signed with the key %q, which this build of Lumen does not trust", f.KeyID)
	}
	body, err := base64.StdEncoding.DecodeString(f.Payload)
	sig, err2 := base64.StdEncoding.DecodeString(f.Signature)
	if err != nil || err2 != nil || len(sig) != ed25519.SignatureSize {
		return License{}, bad("the licence file is damaged")
	}
	if !ed25519.Verify(pub, message(body), sig) {
		return License{}, bad("the signature does not match: the file was changed, or it was not issued for this product")
	}
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		return License{}, bad("the licence file is damaged")
	}
	if err := p.Validate(); err != nil {
		return License{}, err
	}
	sort.Strings(p.Editions)
	return License{Payload: p, KeyID: f.KeyID}, nil
}

// State is where a licence stands at a moment in time.
type State string

const (
	StateNone     State = "none"     // no licence: the Community edition
	StateValid    State = "valid"    // in force
	StateExpiring State = "expiring" // in force, ends within 30 days
	StateGrace    State = "grace"    // has ended less than 30 days ago: Enterprise still works, with a warning
	StateExpired  State = "expired"  // ended more than 30 days ago: Enterprise features are off
	StateInvalid  State = "invalid"  // a stored licence that no longer verifies
)

func (l License) State(now time.Time) State {
	switch {
	case now.Before(l.Expires.Add(-ExpiringSoon)):
		return StateValid
	case now.Before(l.Expires):
		return StateExpiring
	case now.Before(l.Expires.Add(Grace)):
		return StateGrace
	}
	return StateExpired
}

// DaysLeft is the number of days until the licence ends, counting a started day as a day (365 right after a licence
// for a year is issued, 1 during the last day), and negative after it has ended.
func (l License) DaysLeft(now time.Time) int {
	d := l.Expires.Sub(now)
	if d >= 0 {
		return int((d + 24*time.Hour - 1) / (24 * time.Hour))
	}
	return -int((-d + 24*time.Hour - 1) / (24 * time.Hour))
}

func (l License) Has(edition string) bool {
	for _, e := range l.Editions {
		if e == edition {
			return true
		}
	}
	return false
}

// Active tells whether the licence's features work now: in force, or within the grace period.
func (l License) Active(now time.Time) bool { return l.State(now) != StateExpired }

// Warnings compares use with the licence's limits. The limits are soft: this only produces text.
func (l License) Warnings(hosts, tenants int) []string {
	var w []string
	check := func(what string, n, limit int) {
		switch {
		case limit <= 0:
		case n > limit:
			w = append(w, fmt.Sprintf("%d %s are in use but the licence covers %d. Nothing is blocked; contact your supplier to extend it.", n, what, limit))
		case n*10 >= limit*9:
			w = append(w, fmt.Sprintf("%d of the %d %s in the licence are in use.", n, limit, what))
		}
	}
	check("hosts", hosts, l.Limits.Hosts)
	check("tenants", tenants, l.Limits.Tenants)
	return w
}

// Unverified reads the payload of a licence file WITHOUT checking the signature. It is only for showing what a file
// says (the issuer's inspect command); never use it to decide anything.
func Unverified(data []byte) (Payload, error) {
	var f File
	var p Payload
	if err := json.Unmarshal(data, &f); err != nil || f.Format != formatName {
		return p, bad("that is not a Lumen licence file")
	}
	body, err := base64.StdEncoding.DecodeString(f.Payload)
	if err != nil || json.Unmarshal(body, &p) != nil {
		return p, bad("the licence file is damaged")
	}
	return p, nil
}

// ---- keys ----

// NewKeyPair makes a signing key pair.
func NewKeyPair() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

func EncodeKey(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func ParsePublicKey(text string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, bad("that is not a public key")
	}
	return ed25519.PublicKey(b), nil
}

func ParsePrivateKey(text string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, bad("that is not a private key")
	}
	return ed25519.PrivateKey(b), nil
}

// LoadKeys reads every <id>.pub file of a directory.
func LoadKeys(fsys fs.FS, dir string) (Keys, error) {
	ents, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	keys := Keys{}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".pub") {
			continue
		}
		b, err := fs.ReadFile(fsys, dir+"/"+name)
		if err != nil {
			return nil, err
		}
		pub, err := ParsePublicKey(string(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		keys[strings.TrimSuffix(name, ".pub")] = pub
	}
	return keys, nil
}
