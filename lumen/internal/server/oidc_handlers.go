package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/danielingemar/lumen/internal/audit"
	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/oidc"
	"github.com/danielingemar/lumen/internal/perm"
)

// WithOIDC turns on sign-in with an OpenID Connect provider (when it is set up under Settings).
func (s *Server) WithOIDC(o *oidc.Service) *Server { s.oidc = o; return s }

func (s *Server) oidcRoutes(mux *http.ServeMux) {
	if s.oidc == nil {
		return
	}
	mux.HandleFunc("GET /auth/oidc/login", s.oidcLogin)
	mux.HandleFunc("GET /auth/oidc/callback", s.oidcCallback)
	mux.HandleFunc("GET /api/v1/auth/providers", s.authProviders)
	mux.Handle("GET /api/v1/settings/oidc", s.need(perm.Settings, false, s.getOIDC))
	mux.Handle("PUT /api/v1/settings/oidc", s.need(perm.Settings, true, s.putOIDC))
	mux.Handle("POST /api/v1/settings/oidc/test", s.need(perm.Settings, true, s.testOIDC))
}

// authProviders tells the login page which buttons to show. Public: it is needed before anybody has signed in.
func (s *Server) authProviders(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	if c, ok := s.oidc.Load(r.Context()); ok && c.Enabled && c.Issuer != "" {
		out["oidc"] = map[string]string{"label": c.Label}
	}
	writeJSON(w, out)
}

func (s *Server) oidcFail(w http.ResponseWriter, r *http.Request, tenant, who, why string) {
	if log := s.auditLog(); log != nil {
		_ = log.Add(audit.Entry{Tenant: tenant, Actor: who, IP: remoteHost(r), Via: "oidc", Action: "login.failed", Summary: "single sign-on failed", Detail: why})
	}
	s.log.Warn("single sign-on failed", "why", why, "remote", r.RemoteAddr)
	http.Redirect(w, r, "/?oidc_error="+url.QueryEscape(why), http.StatusFound)
}

func (s *Server) oidcLogin(w http.ResponseWriter, r *http.Request) {
	c, ok := s.oidc.Load(r.Context())
	if !ok || !c.Enabled {
		http.Redirect(w, r, "/?oidc_error="+url.QueryEscape("single sign-on is not set up"), http.StatusFound)
		return
	}
	bucket := "oidc:" + remoteHost(r)
	if s.a.Limiter.Blocked(bucket) {
		http.Redirect(w, r, "/?oidc_error="+url.QueryEscape("too many failed attempts, try again in a few minutes"), http.StatusFound)
		return
	}
	to, err := s.oidc.Begin(r.Context(), w, r, c)
	if err != nil {
		s.oidcFail(w, r, c.Tenant, "(single sign-on)", err.Error())
		return
	}
	http.Redirect(w, r, to, http.StatusFound)
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	c, ok := s.oidc.Load(r.Context())
	if !ok || !c.Enabled {
		http.Redirect(w, r, "/?oidc_error="+url.QueryEscape("single sign-on is not set up"), http.StatusFound)
		return
	}
	bucket := "oidc:" + remoteHost(r)
	if s.a.Limiter.Blocked(bucket) {
		http.Redirect(w, r, "/?oidc_error="+url.QueryEscape("too many failed attempts, try again in a few minutes"), http.StatusFound)
		return
	}
	ident, err := s.oidc.Finish(r.Context(), w, r, c)
	if err != nil {
		s.a.Limiter.Fail(bucket)
		s.oidcFail(w, r, c.Tenant, "(single sign-on)", err.Error())
		return
	}
	u, err := s.oidcUser(r, c, ident)
	if err != nil {
		s.a.Limiter.Fail(bucket)
		s.oidcFail(w, r, c.Tenant, firstNonEmpty(ident.Email, ident.Subject), err.Error())
		return
	}
	probe := &capture{h: http.Header{}}
	if !s.tenantUsable(probe, edition.Identity{Tenant: u.Tenant}) {
		s.oidcFail(w, r, u.Tenant, u.Name, strings.TrimSpace(probe.text()))
		return
	}
	s.a.Limiter.Reset(bucket)
	auth.SetCookie(w, r, s.a.IssueSession(u), int(auth.SessionTTL.Seconds()))
	s.auditSignIn(r, u.Name, u.Tenant, true, "oidc")
	http.Redirect(w, r, "/", http.StatusFound)
}

func firstNonEmpty(v ...string) string {
	for _, x := range v {
		if x != "" {
			return x
		}
	}
	return ""
}

// capture takes what tenantUsable would have written to a browser, so that it can be shown on the login page instead.
type capture struct {
	h    http.Header
	body []byte
}

func (c *capture) Header() http.Header         { return c.h }
func (c *capture) WriteHeader(int)             {}
func (c *capture) Write(b []byte) (int, error) { c.body = append(c.body, b...); return len(b), nil }
func (c *capture) text() string {
	t := string(c.body)
	if i := strings.Index(t, `"error":"`); i >= 0 {
		t = t[i+9:]
		if j := strings.Index(t, `"`); j >= 0 {
			t = t[:j]
		}
	}
	return t
}

// oidcUser says which local user a provider identity is: one it was linked to before, one it matches by verified email, or a new one.
func (s *Server) oidcUser(r *http.Request, c oidc.Config, id oidc.Identity) (auth.User, error) {
	if id.Email == "" {
		return auth.User{}, errString("the provider did not give an email address: add the email scope, or an email to the account")
	}
	if id.EmailVerified != nil && !*id.EmailVerified {
		return auth.User{}, errString("the provider says the email address " + id.Email + " is not verified")
	}
	if len(c.Domains) > 0 {
		at := strings.LastIndex(id.Email, "@")
		okDomain := false
		if at > 0 {
			for _, d := range c.Domains {
				if strings.EqualFold(id.Email[at+1:], d) {
					okDomain = true
				}
			}
		}
		if !okDomain {
			return auth.User{}, errString("sign-in with " + id.Email + " is not allowed here (its domain is not on the list)")
		}
	}
	sum := sha256.Sum256([]byte(c.Issuer + "\x00" + id.Subject))
	linkID := hex.EncodeToString(sum[:])
	db := s.oidc.DB
	if d, err := db.Get(r.Context(), oidc.CollLinks, linkID); err == nil { // somebody who has signed in this way before
		var l struct{ User string }
		if d.Decode(&l) == nil {
			if u, ok := s.a.Store.GetUser(l.User); ok && u.Tenant == c.Tenant {
				return u, nil
			}
		}
		_ = db.Delete(r.Context(), oidc.CollLinks, linkID) // the user is gone: the link is stale
	}
	link := func(u auth.User) (auth.User, error) {
		return u, db.Put(r.Context(), oidc.CollLinks, linkID, map[string]string{"tenant": c.Tenant, "user": u.Name, "subject": id.Subject}, "")
	}
	name, err := auth.NormalizeUser(id.Email)
	if err != nil && id.Username != "" {
		name, err = auth.NormalizeUser(id.Username)
	}
	if err != nil {
		return auth.User{}, errString("the email address " + id.Email + " cannot be used as a user name here")
	}
	if u, ok := s.a.Store.GetUser(name); ok {
		switch {
		case u.Tenant != c.Tenant:
			return auth.User{}, errString("an account named " + name + " exists elsewhere on this installation")
		case c.LinkByEmail && id.EmailVerified != nil && *id.EmailVerified:
			return link(u) // the provider vouches for the address, and an administrator chose to trust that
		}
		return auth.User{}, errString("an account named " + name + " already exists and is not linked to this sign-in. Ask an administrator, or sign in with your password")
	}
	if !c.AutoCreate {
		return auth.User{}, errString("there is no account for " + name + ". Ask an administrator to create one with that name")
	}
	group := c.DefaultGroup
	if group == "" {
		group = "user"
	}
	// the password is made up and never shown: this account is entered through the provider (an administrator can set one)
	if err := s.a.Store.CreateUserIn(name, c.Tenant, auth.RandomToken(24), group); err != nil {
		return auth.User{}, errString("the account could not be made: " + err.Error())
	}
	u, _ := s.a.Store.GetUser(name)
	if log := s.auditLog(); log != nil {
		_ = log.Add(audit.Entry{Tenant: c.Tenant, Actor: name, IP: remoteHost(r), Via: "oidc", Action: "user.create", Target: name, Summary: "an account was made at the first sign-in with single sign-on", Detail: "group " + group})
	}
	return link(u)
}

type errString string

func (e errString) Error() string { return string(e) }

// ---- the setting ----

var domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

func (s *Server) oidcOut(r *http.Request, id edition.Identity) map[string]any {
	c, _ := s.oidc.Load(r.Context())
	var groups []string
	for _, g := range s.a.Store.ListGroups(id.Tenant) {
		groups = append(groups, g.ID)
	}
	return map[string]any{"config": map[string]any{"enabled": c.Enabled, "issuer": c.Issuer, "client_id": c.ClientID, "label": c.Label, "domains": nz(c.Domains), "auto_create": c.AutoCreate, "default_group": c.DefaultGroup, "link_by_email": c.LinkByEmail},
		"has_secret": c.Secret != "", "redirect_uri": s.oidc.RedirectURI(), "groups": nz(groups)}
}

func nz(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

func (s *Server) getOIDC(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if !s.ownerOnly(w, id) {
		return
	}
	writeJSON(w, s.oidcOut(r, id))
}

type oidcIn struct {
	Enabled      bool     `json:"enabled"`
	Issuer       string   `json:"issuer"`
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	Label        string   `json:"label"`
	Domains      []string `json:"domains"`
	AutoCreate   bool     `json:"auto_create"`
	DefaultGroup string   `json:"default_group"`
	LinkByEmail  bool     `json:"link_by_email"`
}

func (s *Server) putOIDC(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if !s.ownerOnly(w, id) {
		return
	}
	var in oidcIn
	if !readJSON(w, r, &in) {
		return
	}
	prev, _ := s.oidc.Load(r.Context())
	c := oidc.Config{Enabled: in.Enabled, Issuer: strings.TrimRight(strings.TrimSpace(in.Issuer), "/"), ClientID: strings.TrimSpace(in.ClientID), Label: strings.TrimSpace(in.Label),
		AutoCreate: in.AutoCreate, DefaultGroup: strings.TrimSpace(in.DefaultGroup), LinkByEmail: in.LinkByEmail, Tenant: id.Tenant, UpdatedBy: id.User, Secret: prev.Secret}
	bad := func(msg string) { writeErr(w, http.StatusBadRequest, msg) }
	if c.Label == "" {
		c.Label = "single sign-on"
	}
	if len([]rune(c.Label)) > 40 || len(c.ClientID) > 300 || strings.ContainsAny(c.Label+c.ClientID, "\x00\r\n") {
		bad("the button text is at most 40 characters, and the client id at most 300, without line breaks")
		return
	}
	for _, d := range in.Domains {
		d = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(d), "@")))
		if d == "" {
			continue
		}
		if !domainRe.MatchString(d) {
			bad("\"" + d + "\" is not a domain (write for example example.com)")
			return
		}
		c.Domains = append(c.Domains, d)
	}
	if c.DefaultGroup != "" {
		if _, ok := s.a.Store.GetGroup(id.Tenant, c.DefaultGroup); !ok {
			bad("there is no group \"" + c.DefaultGroup + "\" to put new accounts in")
			return
		}
	}
	if c.AutoCreate && (c.DefaultGroup == "admin") && len(c.Domains) == 0 {
		bad("making administrators of everybody who signs in is not allowed: choose another group, or list the email domains that may sign in")
		return
	}
	if c.Issuer != "" {
		if err := s.oidc.CheckURL(c.Issuer); err != nil {
			bad(err.Error())
			return
		}
	}
	if c.Enabled {
		if c.Issuer == "" || c.ClientID == "" || (in.ClientSecret == "" && prev.Secret == "") {
			bad("to turn it on, give the issuer address, the client id and the client secret")
			return
		}
		if s.oidc.RedirectURI() == "" {
			bad("LUMEN_PUBLIC_URL is not set, so Lumen does not know the address to register at the provider. Set it, restart, and try again")
			return
		}
		s.oidc.Forget()
		if _, err := s.oidc.Discover(r.Context(), c); err != nil {
			bad("it was not turned on: " + err.Error())
			return
		}
	}
	if err := s.oidc.Save(r.Context(), c, in.ClientSecret); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "could not save the setting")
		return
	}
	s.oidc.Forget()
	writeJSON(w, s.oidcOut(r, id))
}

// testOIDC asks the provider about itself, without saving anything.
func (s *Server) testOIDC(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	if !s.ownerOnly(w, id) {
		return
	}
	var in oidcIn
	if !readJSON(w, r, &in) {
		return
	}
	s.oidc.Forget()
	d, err := s.oidc.Discover(r.Context(), oidc.Config{Issuer: strings.TrimRight(strings.TrimSpace(in.Issuer), "/")})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "authorization_endpoint": d.AuthURL, "redirect_uri": s.oidc.RedirectURI()})
}
