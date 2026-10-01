package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/dashboards"
	"github.com/danielingemar/lumen/internal/docstore"
)

func rbacServer(t *testing.T) (*httptest.Server, *auth.Auth) {
	f, _ := docstore.OpenFile(t.TempDir())
	st, _ := auth.Open(f, 0)
	st.CreateUser("admin", "acme", "admins-long-password") // built-in admin group
	st.CreateUserIn("reader", "acme", "readers-long-password", "user")
	st.CreateUser("other", "globex", "others-long-password")
	a := auth.New(st, nil, false)
	ts := httptest.NewServer(New(&fakeStore{}, a, slog.New(slog.NewTextHandler(io.Discard, nil))).WithAuth(a).WithDashboards(dashboards.New(f)).Handler())
	t.Cleanup(ts.Close)
	return ts, a
}

func code(c *http.Client, method, url, body string) int {
	r, _ := do(c, method, url, body)
	return r.StatusCode
}

func TestBuiltinUserGroupIsReadOnly(t *testing.T) {
	ts, _ := rbacServer(t)
	rd := client()
	login(t, rd, ts, "reader", "readers-long-password")
	for _, p := range []string{"/api/v1/traces", "/api/v1/logs", "/api/v1/dashboards", "/api/v1/services", "/api/v1/series?source=metric&name=x"} {
		if c := code(rd, "GET", ts.URL+p, ""); c != 200 {
			t.Errorf("a user must read %s, got %d", p, c)
		}
	}
	for _, x := range []struct{ m, p, b string }{
		{"POST", "/api/v1/dashboards", `{"name":"x","body":{"panels":[]}}`},
		{"POST", "/api/v1/keys", `{"name":"x"}`}, {"GET", "/api/v1/keys", ""},
		{"GET", "/api/v1/users", ""}, {"POST", "/api/v1/users", `{"name":"n","group":"admin"}`},
		{"GET", "/api/v1/groups", ""}, {"POST", "/api/v1/groups", `{"name":"g","perms":{}}`},
		{"GET", "/api/v1/traces?archive=1", ""},
	} {
		if c := code(rd, x.m, ts.URL+x.p, x.b); c != 403 {
			t.Errorf("a read-only user must not %s %s (got %d)", x.m, x.p, c)
		}
	}
	var me map[string]any
	json.NewDecoder(get(rd, ts.URL+"/api/v1/me").Body).Decode(&me)
	if me["group"] != "user" || me["group_name"] != "User" || me["perms"].(map[string]any)["dashboards"] != "read" {
		t.Fatalf("me: %+v", me)
	}
}

func TestAdminManagesUsersAndCustomGroups(t *testing.T) {
	ts, _ := rbacServer(t)
	ad, rd := client(), client()
	login(t, ad, ts, "admin", "admins-long-password")
	// a custom group: read traces and logs, write dashboards, nothing else
	r, b := do(ad, "POST", ts.URL+"/api/v1/groups", `{"name":"Support","description":"helpdesk","perms":{"traces":"read","logs":"read","dashboards":"write","metrics":"none","users":"write"}}`)
	var g auth.Group
	json.Unmarshal(b, &g)
	if r.StatusCode != 200 || g.Tenant != "acme" || g.Perms["users"] != "write" || g.Perms["keys"] != "none" {
		t.Fatalf("create group: %d %s", r.StatusCode, b)
	}
	// non-writable areas are stored as read, unknown areas and levels are refused
	_, b = do(ad, "POST", ts.URL+"/api/v1/groups", `{"name":"Wr","perms":{"traces":"write"}}`)
	var g2 auth.Group
	json.Unmarshal(b, &g2)
	if g2.Perms["traces"] != "read" {
		t.Fatalf("write on traces must become read: %+v", g2.Perms)
	}
	for _, bad := range []string{`{"name":"x","perms":{"bogus":"read"}}`, `{"name":"x","perms":{"logs":"admin"}}`, `{"name":"","perms":{}}`, `{"name":"support","perms":{}}`} {
		if c := code(ad, "POST", ts.URL+"/api/v1/groups", bad); c != 400 {
			t.Errorf("%s must be 400, got %d", bad, c)
		}
	}
	// create a user in it; a password is generated and returned once
	r, b = do(ad, "POST", ts.URL+"/api/v1/users", `{"name":"Sara","group":"`+g.ID+`"}`)
	var created struct {
		Password string
		User     userOut
	}
	json.Unmarshal(b, &created)
	if r.StatusCode != 200 || len(created.Password) < 12 || created.User.GroupName != "Support" {
		t.Fatalf("create user: %d %s", r.StatusCode, b)
	}
	if c := code(ad, "POST", ts.URL+"/api/v1/users", `{"name":"x","group":"nope"}`); c != 400 {
		t.Fatalf("unknown group must be 400, got %d", c)
	}
	// the new user has exactly the custom permissions
	login(t, rd, ts, "sara", created.Password)
	checks := map[string]int{"/api/v1/traces": 200, "/api/v1/logs": 200, "/api/v1/dashboards": 200, "/api/v1/keys": 403, "/api/v1/series?source=metric&name=x": 403, "/api/v1/metrics/names": 403, "/api/v1/series?source=logs": 200}
	for p, want := range checks {
		if c := code(rd, "GET", ts.URL+p, ""); c != want {
			t.Errorf("custom user GET %s: want %d got %d", p, want, c)
		}
	}
	if c := code(rd, "POST", ts.URL+"/api/v1/dashboards", `{"name":"mine","body":{"panels":[]}}`); c != 200 {
		t.Fatalf("custom user may write dashboards, got %d", c)
	}
	// a group change takes effect immediately for the logged-in user
	do(ad, "PUT", ts.URL+"/api/v1/groups/"+g.ID, `{"name":"Support","perms":{"traces":"read","users":"write"}}`)
	if c := code(rd, "GET", ts.URL+"/api/v1/logs", ""); c != 403 {
		t.Fatalf("removing logs from the group must apply at once, got %d", c)
	}
	// deleting a group that has members is refused; built-ins cannot be changed or deleted
	if c := code(ad, "DELETE", ts.URL+"/api/v1/groups/"+g.ID, ""); c != 400 {
		t.Fatalf("group with members must not be deletable, got %d", c)
	}
	for _, id := range []string{"admin", "user"} {
		if code(ad, "DELETE", ts.URL+"/api/v1/groups/"+id, "") != 400 || code(ad, "PUT", ts.URL+"/api/v1/groups/"+id, `{"name":"x","perms":{}}`) != 400 {
			t.Fatalf("built-in group %s must be immutable", id)
		}
	}
	// reset password: new one works, old sessions end
	_, b = do(ad, "PUT", ts.URL+"/api/v1/users/sara", `{"password":"reset"}`)
	var rs struct{ Password string }
	json.Unmarshal(b, &rs)
	if rs.Password == "" || code(rd, "GET", ts.URL+"/api/v1/traces", "") != 401 {
		t.Fatalf("reset must return a password and end the old session: %s", b)
	}
	// move her to the built-in user group, then delete her
	do(ad, "PUT", ts.URL+"/api/v1/users/sara", `{"group":"user"}`)
	if code(ad, "DELETE", ts.URL+"/api/v1/groups/"+g.ID, "") != 200 {
		t.Fatal("an empty custom group can be deleted")
	}
	if code(ad, "DELETE", ts.URL+"/api/v1/users/sara", "") != 200 {
		t.Fatal("admin must be able to delete a user")
	}
}

func TestUserAdminSafetyAndIsolation(t *testing.T) {
	ts, a := rbacServer(t)
	ad, other := client(), client()
	login(t, ad, ts, "admin", "admins-long-password")
	login(t, other, ts, "other", "others-long-password")
	if c := code(ad, "DELETE", ts.URL+"/api/v1/users/admin", ""); c != 400 {
		t.Fatalf("you cannot delete yourself: %d", c)
	}
	if c := code(ad, "PUT", ts.URL+"/api/v1/users/admin", `{"group":"user"}`); c != 400 {
		t.Fatalf("you cannot change your own group: %d", c)
	}
	// a second admin exists; the first may delete it, but then the first is the last admin and nobody can remove them
	do(ad, "POST", ts.URL+"/api/v1/users", `{"name":"admin2","password":"admin2-long-password","group":"admin"}`)
	ad2 := client()
	login(t, ad2, ts, "admin2", "admin2-long-password")
	if c := code(ad2, "PUT", ts.URL+"/api/v1/users/admin", `{"group":"user"}`); c != 200 {
		t.Fatalf("with two admins one may demote the other: %d", c)
	}
	if c := code(ad, "GET", ts.URL+"/api/v1/users", ""); c != 403 {
		t.Fatalf("demoted admin must lose access at once: %d", c)
	}
	if c := code(ad2, "PUT", ts.URL+"/api/v1/users/admin", `{"group":"admin"}`); c != 200 {
		t.Fatalf("promote back: %d", c)
	}
	if c := code(ad2, "DELETE", ts.URL+"/api/v1/users/admin", ""); c != 200 {
		t.Fatalf("delete the other admin: %d", c)
	}
	if _, ok := a.Store.GetUser("admin"); ok {
		t.Fatal("deleted")
	}
	// tenant isolation: another tenant's users and groups are invisible
	_, b := do(other, "GET", ts.URL+"/api/v1/users", "")
	if strings.Contains(string(b), "reader") || strings.Contains(string(b), "admin2") {
		t.Fatalf("other tenant sees acme users: %s", b)
	}
	if c := code(other, "PUT", ts.URL+"/api/v1/users/reader", `{"group":"admin"}`); c != 404 {
		t.Fatalf("other tenant must not modify acme users: %d", c)
	}
	if c := code(other, "DELETE", ts.URL+"/api/v1/users/reader", ""); c != 404 {
		t.Fatalf("other tenant must not delete acme users: %d", c)
	}
	_, b = do(ad2, "POST", ts.URL+"/api/v1/groups", `{"name":"Secret","perms":{"traces":"read"}}`)
	var g auth.Group
	json.Unmarshal(b, &g)
	if c := code(other, "PUT", ts.URL+"/api/v1/groups/"+g.ID, `{"name":"x","perms":{}}`); c != 400 {
		t.Fatalf("other tenant must not edit acme's group: %d", c)
	}
	if code(other, "POST", ts.URL+"/api/v1/users", `{"name":"intruder","group":"`+g.ID+`"}`) != 400 {
		t.Fatal("a user must not be put into another tenant's group")
	}
	// an API key (agent) never manages users and has no archive access
	plain, _, _ := a.Store.CreateKey("acme", "agent")
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/users", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 && resp.StatusCode != 403 {
		t.Fatalf("an API key must not list users: %d", resp.StatusCode)
	}
}

func TestLegacyUsersWithoutGroupAreAdmins(t *testing.T) {
	f, _ := docstore.OpenFile(t.TempDir())
	st, _ := auth.Open(f, 0)
	h, _ := auth.HashPassword("legacy-long-password")
	f.Create(context.Background(), "users", "old", map[string]any{"name": "old", "tenant": "acme", "hash": h, "created": "2026-01-01T00:00:00Z"}) // no "group" field, as before RBAC
	u, _ := st.GetUser("old")
	if st.PermsFor(u)["users"] != "write" || auth.EffectiveGroup(u) != "admin" {
		t.Fatal("accounts created before groups existed must keep full access")
	}
}
