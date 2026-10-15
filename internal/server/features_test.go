package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inmagma/lumen/internal/auth"
	"github.com/inmagma/lumen/internal/billing"
	"github.com/inmagma/lumen/internal/dashboards"
	"github.com/inmagma/lumen/internal/docstore"
	"github.com/inmagma/lumen/internal/license"
	"github.com/inmagma/lumen/internal/oidc"
	"github.com/inmagma/lumen/internal/perm"
	"github.com/inmagma/lumen/internal/registry"
	"github.com/inmagma/lumen/internal/secretbox"
)

// enterpriseLicense is a manager holding a valid Enterprise licence, for tests of the features it unlocks.
func enterpriseLicense(t *testing.T, db docstore.Backend) *license.Manager {
	t.Helper()
	pub, priv, _ := license.NewKeyPair()
	m := license.NewManager(db, license.Keys{"main": pub}, "")
	b, err := license.Sign(priv, "main", license.Payload{ID: "T", Customer: "Test AB", Editions: []string{license.Enterprise}, Issued: time.Now(), Expires: time.Now().AddDate(1, 0, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Set(b, "test"); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestEnterpriseFeaturesAreLockedWithoutALicence(t *testing.T) {
	f, _ := docstore.OpenFile(t.TempDir())
	st, _ := auth.Open(f, 0)
	st.CreateUser("admin", "acme", "admins-long-password")
	a := auth.New(st, nil, false)
	box, _ := secretbox.New("k")
	pub, priv, _ := license.NewKeyPair()
	mgr := license.NewManager(f, license.Keys{"main": pub}, "")
	mgr.Load()
	svc := &oidc.Service{DB: f, Box: box, PublicURL: "http://lumen.test", Secret: st.Secret}
	srv := New(&fakeStore{}, a, slog.New(slog.NewTextHandler(io.Discard, nil))).WithAuth(a).WithDashboards(dashboards.New(f)).WithOIDC(svc).WithRegistry(registry.New(f, box)).WithLicense(mgr).WithOwnerTenant("acme").
		WithBilling(&Billing{Svc: billing.New(f), DB: f})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	ad := client()
	login(t, ad, ts, "admin", "admins-long-password")

	feat := func() map[string]bool {
		_, b := do(ad, "GET", ts.URL+"/api/v1/me", "")
		var me struct{ Features map[string]bool }
		json.Unmarshal(b, &me)
		return me.Features
	}
	if ft := feat(); ft["groups"] || ft["sso"] || ft["billing"] {
		t.Fatalf("no licence, no features: %v", ft)
	}

	// any number of users in the built-in groups is Community
	for _, n := range []string{"anna", "bo", "cia", "dag"} {
		if c := code(ad, "POST", ts.URL+"/api/v1/users", `{"name":"`+n+`","password":"long-enough-password","group":"user"}`); c != 200 {
			t.Fatalf("user %s: %d", n, c)
		}
	}
	if c := code(ad, "POST", ts.URL+"/api/v1/users", `{"name":"eva","password":"long-enough-password","group":"admin"}`); c != 200 {
		t.Fatalf("a second administrator is Community: %d", c)
	}
	if c := code(ad, "GET", ts.URL+"/api/v1/groups", ""); c != 200 {
		t.Fatalf("the built-in groups can be listed: %d", c)
	}
	// custom groups, SSO and billing are not
	if c := code(ad, "POST", ts.URL+"/api/v1/groups", `{"name":"Support","perms":{"logs":"read"}}`); c != 403 {
		t.Fatalf("custom group without a licence: %d", c)
	}
	if c := code(ad, "PUT", ts.URL+"/api/v1/settings/oidc", `{"enabled":true,"issuer":"https://idp.example","client_id":"x","secret":"y"}`); c != 403 {
		t.Fatalf("enabling SSO without a licence: %d", c)
	}
	if c := code(ad, "POST", ts.URL+"/api/v1/settings/oidc/test", `{"issuer":"https://idp.example"}`); c != 403 {
		t.Fatalf("testing SSO without a licence: %d", c)
	}
	if c := code(ad, "GET", ts.URL+"/api/v1/billing", ""); c != 403 {
		t.Fatalf("billing without a licence: %d", c)
	}
	if c := code(ad, "PUT", ts.URL+"/api/v1/billing", `{}`); c != 403 {
		t.Fatalf("billing prices without a licence: %d", c)
	}
	if c := code(ad, "GET", ts.URL+"/api/v1/billing/report", ""); c != 403 {
		t.Fatalf("billing report without a licence: %d", c)
	}
	// the sign-in page offers no SSO button, and the endpoint says why
	_, b := do(client(), "GET", ts.URL+"/api/v1/auth/providers", "")
	if strings.Contains(string(b), "oidc") {
		t.Fatalf("no SSO button without a licence: %s", b)
	}
	nr := client()
	nr.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if r, _ := nr.Get(ts.URL + "/auth/oidc/login"); r == nil || !strings.Contains(r.Header.Get("Location"), "oidc_error") {
		t.Fatalf("SSO login must be refused with a message")
	}

	// with a licence everything opens, and a custom group can be given to a user
	raw, _ := license.Sign(priv, "main", license.Payload{ID: "L", Customer: "ACME", Editions: []string{license.Enterprise}, Issued: time.Now(), Expires: time.Now().AddDate(1, 0, 0)})
	if _, err := mgr.Set(raw, "test"); err != nil {
		t.Fatal(err)
	}
	if ft := feat(); !ft["groups"] || !ft["sso"] || !ft["billing"] {
		t.Fatalf("licensed: %v", ft)
	}
	_, b = do(ad, "POST", ts.URL+"/api/v1/groups", `{"name":"Support","perms":{"logs":"read"}}`)
	var g auth.Group
	json.Unmarshal(b, &g)
	if g.ID == "" {
		t.Fatalf("group not created: %s", b)
	}
	if c := code(ad, "PUT", ts.URL+"/api/v1/users/anna", `{"group":"`+g.ID+`"}`); c != 200 {
		t.Fatalf("assign custom group: %d", c)
	}
	if c := code(ad, "GET", ts.URL+"/api/v1/billing", ""); c != 200 {
		t.Fatalf("billing with a licence: %d", c)
	}

	// when the licence ends: nobody is locked out and the groups already made keep their permissions;
	// only new custom groups, SSO and billing stop
	if err := mgr.Remove(); err != nil {
		t.Fatal(err)
	}
	if feat()["groups"] {
		t.Fatal("licence removed")
	}
	if p := st.PermsFor(mustUser(t, st, "anna")); p["logs"] != perm.Read || p["hosts"] != perm.None {
		t.Fatalf("an existing group must keep being enforced: %v", p)
	}
	if c := code(ad, "PUT", ts.URL+"/api/v1/users/bo", `{"group":"`+g.ID+`"}`); c != 403 {
		t.Fatalf("no new assignments to a custom group: %d", c)
	}
	if c := code(ad, "PUT", ts.URL+"/api/v1/users/anna", `{"group":"user"}`); c != 200 {
		t.Fatalf("moving a user back to a built-in group always works: %d", c)
	}
	if c := code(ad, "DELETE", ts.URL+"/api/v1/groups/"+g.ID, ""); c != 200 {
		t.Fatalf("deleting a group always works: %d", c)
	}
	if c := code(ad, "PUT", ts.URL+"/api/v1/settings/oidc", `{"enabled":false}`); c == 403 {
		t.Fatal("switching SSO off must always be possible")
	}
}

func mustUser(t *testing.T, st *auth.Store, name string) auth.User {
	u, ok := st.GetUser(name)
	if !ok {
		t.Fatalf("no user %s", name)
	}
	return u
}
