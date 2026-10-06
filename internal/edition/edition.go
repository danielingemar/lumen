// Package edition is the open-core seam. The community build ships the defaults below.
// Enterprise features live under /ee (separate license, build tag "enterprise") and
// replace these hooks from an init() function, so the open-source core never imports /ee.
package edition

import (
	"errors"
	"net/http"
	"time"
)

// Name is "community" or "enterprise".
var Name = "community"

// Identity is the authenticated caller. Tenant scopes every read and write.
type Identity struct {
	Tenant string
	User   string   // empty for API keys
	Roles  []string // used by enterprise RBAC
	// Session is true when the identity came from a browser login cookie rather than an API key.
	// Cookie identities may read and manage keys but may not ingest telemetry.
	Session bool
	// ViaKey is true for a browser session opened with an API key instead of a username and password.
	// Such sessions can read data and use dashboards but cannot manage keys or passwords.
	ViaKey bool
	// Perms maps an area (dashboards, traces, logs, metrics, hosts, keys, users, backups) to none|read|write.
	Perms map[string]string
	// Operator is set while an operator of the installation is inside a tenant for support: it is the operator's name.
	// Everything done then is on record, and the identity is that of a tenant administrator (read-only unless asked).
	Operator    string
	Acting      bool
	ActingUntil time.Time
	ActingWrite bool
}

// Can reports whether the identity may read (or write) an area.
func (i Identity) Can(area string, write bool) bool {
	l := i.Perms[area]
	if write {
		return l == "write"
	}
	return l == "read" || l == "write"
}

var ErrUnauthorized = errors.New("unauthorized")

// Authenticator resolves a request to an Identity. Enterprise swaps in SSO/OIDC/SAML.
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}

// Authorizer decides whether an identity may perform an action. Community allows everything
// within the caller's own tenant; enterprise adds role-based access control.
type Authorizer interface {
	Allow(id Identity, action string) bool
}

type allowAll struct{}

func (allowAll) Allow(Identity, string) bool { return true }

// Hooks replaced by /ee. Do not reassign from community code.
var (
	NewAuthenticator            = func(keys map[string]string) Authenticator { return &apiKeyAuth{keys: keys} }
	Authz            Authorizer = allowAll{}
)

type apiKeyAuth struct{ keys map[string]string }

func (a *apiKeyAuth) Authenticate(r *http.Request) (Identity, error) {
	if len(a.keys) == 0 { // dev mode: no keys configured
		return Identity{Tenant: "default"}, nil
	}
	key := r.Header.Get("X-API-Key")
	if h := r.Header.Get("Authorization"); len(h) > 7 && h[:7] == "Bearer " {
		key = h[7:]
	}
	if t, ok := a.keys[key]; ok && key != "" {
		return Identity{Tenant: t}, nil
	}
	return Identity{}, ErrUnauthorized
}
