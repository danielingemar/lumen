package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/alerts"
	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/dashboards"
	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/license"
	"github.com/danielingemar/lumen/internal/secretbox"
)

// a stand-in for an Enterprise channel type, so the gate can be tested without the enterprise build
type fakeEnt struct{}

func (fakeEnt) Type() string    { return "zz-enterprise-test" }
func (fakeEnt) Label() string   { return "Enterprise test channel" }
func (fakeEnt) Edition() string { return "enterprise" }
func (fakeEnt) Fields() []alerts.Field {
	return []alerts.Field{{Key: "target", Label: "Target", Kind: "text", Required: true}}
}
func (fakeEnt) Validate(alerts.Config) error { return nil }
func (fakeEnt) Send(context.Context, alerts.Deps, alerts.Config, alerts.Message) (alerts.Result, error) {
	return alerts.Result{}, nil
}

func init() { alerts.Default.Register(fakeEnt{}) }

type licEnv struct {
	ts   *httptest.Server
	mgr  *license.Manager
	priv ed25519.PrivateKey
	clk  *struct{ t time.Time }
}

func licServer(t *testing.T) *licEnv {
	f, _ := docstore.OpenFile(t.TempDir())
	st, _ := auth.Open(f, 0)
	st.CreateUser("admin", "acme", "admins-long-password")
	st.CreateUserIn("reader", "acme", "readers-long-password", "user")
	st.CreateUser("globexadmin", "globex", "globex-long-password") // another customer on the same installation
	a := auth.New(st, nil, false)
	box, _ := secretbox.New("k")
	pub, priv, _ := license.NewKeyPair()
	clk := &struct{ t time.Time }{t: time.Now()}
	mgr := license.NewManager(f, license.Keys{"main": pub}, "")
	mgr.Now = func() time.Time { return clk.t }
	mgr.Load()
	eng := &alerts.Engine{License: mgr, DB: f, Box: box, Eval: &alerts.Evaluator{Q: &alertQ{}, St: noStatus{}}, Now: func() time.Time { return clk.t }}
	ts := httptest.NewServer(New(&fakeStore{}, a, slog.New(slog.NewTextHandler(io.Discard, nil))).WithAuth(a).WithDashboards(dashboards.New(f)).WithAlerts(eng).WithLicense(mgr).WithOwnerTenant("acme").Handler())
	t.Cleanup(ts.Close)
	return &licEnv{ts, mgr, priv, clk}
}

func (v *licEnv) issue(t *testing.T, mod func(*license.Payload)) string {
	p := license.Payload{ID: "L-1", Customer: "ACME AB", Editions: []string{license.Enterprise}, Issued: v.clk.t, Expires: v.clk.t.AddDate(1, 0, 0), Limits: license.Limits{Hosts: 5}}
	if mod != nil {
		mod(&p)
	}
	b, err := license.Sign(v.priv, "main", p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type clientT struct{ c *http.Client }

func newClient(t *testing.T, ts *httptest.Server, user, pw string) *clientT {
	c := client()
	login(t, c, ts, user, pw)
	return &clientT{c}
}

func TestLicenceSettings(t *testing.T) {
	v := licServer(t)
	login := func(u, p string) (c *clientT) { c = newClient(t, v.ts, u, p); return }
	ad, rd, gx := login("admin", "admins-long-password"), login("reader", "readers-long-password"), login("globexadmin", "globex-long-password")
	// no licence: Community
	_, b := do(ad.c, "GET", v.ts.URL+"/api/v1/settings/license", "")
	var out struct {
		License  license.Info
		Warnings []string
	}
	json.Unmarshal(b, &out)
	if out.License.State != license.StateNone || out.License.Trusted != 1 {
		t.Fatalf("%s", b)
	}
	// who may see and change it
	if code(rd.c, "GET", v.ts.URL+"/api/v1/settings/license", "") != 403 || code(rd.c, "PUT", v.ts.URL+"/api/v1/settings/license", `{"license":"x"}`) != 403 {
		t.Fatal("a user without the Settings permission cannot see or change the licence")
	}
	good := v.issue(t, nil)
	body, _ := json.Marshal(map[string]string{"license": good})
	for _, m := range []string{"GET", "PUT", "DELETE"} {
		if c := code(gx.c, m, v.ts.URL+"/api/v1/settings/license", string(body)); c != 403 {
			t.Errorf("an administrator of another customer must not %s the installation's licence: %d", m, c)
		}
	}
	// refused files, each with a reason
	for name, data := range map[string]struct{ body, want string }{
		"not a licence": {`{"license":"hello"}`, "not a Lumen licence file"}, "empty": {`{"license":""}`, "not a Lumen licence file"},
		"unknown key": {mustJSON(v.issueWith(t, "other")), "does not trust"},
	} {
		r, b := do(ad.c, "PUT", v.ts.URL+"/api/v1/settings/license", data.body)
		if r.StatusCode != 400 || !strings.Contains(string(b), data.want) {
			t.Errorf("%s: %d %s", name, r.StatusCode, b)
		}
	}
	if v.mgr.State() != license.StateNone {
		t.Fatal("a refused licence changes nothing")
	}
	// the types say which channels need a licence, and creating one is refused
	typeLocked := func() bool {
		_, b := do(ad.c, "GET", v.ts.URL+"/api/v1/notifications/types", "")
		var tp struct {
			Data []struct {
				Type, Edition string
				Locked        bool
			}
		}
		json.Unmarshal(b, &tp)
		for _, x := range tp.Data {
			if x.Type == "zz-enterprise-test" {
				if x.Edition != "enterprise" {
					t.Fatalf("edition: %+v", x)
				}
				return x.Locked
			}
			if x.Type == "email" && (x.Locked || x.Edition != "community") {
				t.Fatalf("Community types are never locked: %+v", x)
			}
		}
		t.Fatal("type missing")
		return false
	}
	ch := `{"name":"t","type":"zz-enterprise-test","enabled":true,"settings":{"target":"x"}}`
	if !typeLocked() {
		t.Fatal("locked without a licence")
	}
	if r, b := do(ad.c, "POST", v.ts.URL+"/api/v1/notifications/channels", ch); r.StatusCode != 400 || !strings.Contains(string(b), "needs a Lumen Enterprise licence") {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	// install a licence
	r, b := do(ad.c, "PUT", v.ts.URL+"/api/v1/settings/license", string(body))
	json.Unmarshal(b, &out)
	if r.StatusCode != 200 || out.License.State != license.StateValid || out.License.Customer != "ACME AB" || out.License.DaysLeft < 364 || len(out.License.Editions) != 1 || out.License.Limits.Hosts != 5 {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	if typeLocked() {
		t.Fatal("unlocked by the licence")
	}
	r, b = do(ad.c, "POST", v.ts.URL+"/api/v1/notifications/channels", ch)
	if r.StatusCode != 200 {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	// /me tells the owner's administrators where the licence stands (for the banner), and nobody else
	_, b = do(ad.c, "GET", v.ts.URL+"/api/v1/me", "")
	if !strings.Contains(string(b), `"license":{"state":"valid"`) || !strings.Contains(string(b), `"customer":"ACME AB"`) {
		t.Fatalf("%s", b)
	}
	_, b = do(rd.c, "GET", v.ts.URL+"/api/v1/me", "")
	if strings.Contains(string(b), "license") {
		t.Fatalf("a user without Settings gets no licence information: %s", b)
	}
	_, b = do(gx.c, "GET", v.ts.URL+"/api/v1/me", "")
	if strings.Contains(string(b), "license") {
		t.Fatalf("another customer's administrator gets none either: %s", b)
	}
	// time passes: expiring soon, then grace (still works), then expired (off)
	v.clk.t = v.clk.t.AddDate(1, 0, -10)
	if st := v.mgr.State(); st != license.StateExpiring || typeLocked() {
		t.Fatal(st)
	}
	v.clk.t = v.clk.t.AddDate(0, 0, 15)
	if st := v.mgr.State(); st != license.StateGrace || typeLocked() {
		t.Fatalf("in the grace period Enterprise keeps working: %s", st)
	}
	_, b = do(ad.c, "GET", v.ts.URL+"/api/v1/me", "")
	if !strings.Contains(string(b), `"state":"grace"`) {
		t.Fatalf("%s", b)
	}
	v.clk.t = v.clk.t.AddDate(0, 0, 40)
	if st := v.mgr.State(); st != license.StateExpired || !typeLocked() {
		t.Fatalf("after the grace period it stops: %s", st)
	}
	_, b = do(ad.c, "GET", v.ts.URL+"/api/v1/notifications/channels", "")
	if !strings.Contains(string(b), `"locked":true`) || !strings.Contains(string(b), `"name":"t"`) {
		t.Fatalf("the channel is still there, marked locked, nothing is deleted: %s", b)
	}
	if code(ad.c, "GET", v.ts.URL+"/api/v1/alerts/rules", "") != 200 || code(ad.c, "GET", v.ts.URL+"/api/v1/notifications/types", "") != 200 {
		t.Fatal("Community features keep working after the licence has ended")
	}
	// a renewal is accepted even though the old one has ended; removing it goes back to Community
	v.clk.t = time.Now()
	if r, _ := do(ad.c, "PUT", v.ts.URL+"/api/v1/settings/license", mustJSON(v.issue(t, func(p *license.Payload) { p.ID = "L-2" }))); r.StatusCode != 200 || typeLocked() {
		t.Fatal("renewed")
	}
	r, b = do(ad.c, "DELETE", v.ts.URL+"/api/v1/settings/license", "")
	if r.StatusCode != 200 || v.mgr.State() != license.StateNone || !typeLocked() {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	// soft limits: a warning, nothing blocked
	do(ad.c, "PUT", v.ts.URL+"/api/v1/settings/license", string(body))
	_, b = do(ad.c, "GET", v.ts.URL+"/api/v1/settings/license", "")
	if !strings.Contains(string(b), `"usage":{"hosts":0`) || !strings.Contains(string(b), `"warnings":[]`) {
		t.Fatalf("%s", b)
	}
}

func (v *licEnv) issueWith(t *testing.T, keyID string) string {
	b, err := license.Sign(v.priv, keyID, license.Payload{ID: "L", Customer: "X", Editions: []string{license.Enterprise}, Issued: v.clk.t, Expires: v.clk.t.AddDate(1, 0, 0)})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustJSON(licenseText string) string {
	b, _ := json.Marshal(map[string]string{"license": licenseText})
	return string(b)
}
