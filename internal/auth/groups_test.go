package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/perm"
)

func TestOperatorConsoleOnlyInTheOperatorTenant(t *testing.T) {
	eachStore(t, func(t *testing.T, mk func() *Store, name string) {
		st := mk()
		// a customer's administrator never gets the operator console, however the group was made
		if err := st.CreateUser("boss", "acme", "boss-long-password"); err != nil {
			t.Fatal(err)
		}
		u, _ := st.GetUser("boss")
		for _, g := range st.ListGroups("acme") {
			if g.Perms[perm.Operator] != perm.None {
				t.Fatalf("a customer's %s group must not list the operator console: %v", g.ID, g.Perms)
			}
		}
		if g, _ := st.GetGroup("acme", "admin"); g.Perms[perm.Operator] != perm.None || g.Perms[perm.Users] != perm.Write {
			t.Fatalf("%v", g.Perms)
		}
		if p := st.PermsFor(u); p[perm.Operator] != perm.None || p[perm.Users] != perm.Write {
			t.Fatalf("the built-in admin group of a customer: %v", p)
		}
		if _, err := st.CreateGroup("acme", "Sneaky", "", map[string]string{"operator": "write"}); err == nil || !strings.Contains(err.Error(), "operator tenant") {
			t.Fatalf("a customer cannot make a group that manages tenants: %v", err)
		}
		g, err := st.CreateGroup("acme", "Fine", "", map[string]string{"users": "read"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpdateGroup("acme", g.ID, "Fine", "", map[string]string{"operator": "read"}); err == nil {
			t.Fatal("nor can an existing group be changed into one")
		}
		// in the operator tenant the built-in admin has the console and a custom group may be given it
		if err := st.CreateUser("ops", perm.OperatorTenant, "ops-long-password"); err != nil {
			t.Fatal(err)
		}
		o, _ := st.GetUser("ops")
		if g, _ := st.GetGroup(perm.OperatorTenant, "admin"); g.Perms[perm.Operator] != perm.Write {
			t.Fatalf("the operator tenant's admin group does have it: %v", g.Perms)
		}
		if st.PermsFor(o)[perm.Operator] != perm.Write {
			t.Fatalf("operator admin: %v", st.PermsFor(o))
		}
		if _, err := st.CreateGroup(perm.OperatorTenant, "Support", "", map[string]string{"operator": "read"}); err != nil {
			t.Fatalf("%v", err)
		}
		// even a group stored with the console in another tenant (an old or hand-edited document) is cut off
		if p := perm.ForTenant(map[string]string{"operator": "write", "users": "write"}, "acme"); p["operator"] != "none" || p["users"] != "write" {
			t.Fatalf("%v", p)
		}
		if st.PermsFor(User{Name: "x", Tenant: "acme", Group: "user"})[perm.Operator] != perm.None {
			t.Fatal("users")
		}
	})
}

func TestOperatorActingInsideATenant(t *testing.T) {
	eachStore(t, func(t *testing.T, mk func() *Store, name string) {
		s := mk()
		s.CreateUser("ops", perm.OperatorTenant, "ops-long-password")
		s.CreateUser("boss", "acme", "boss-long-password")
		a := New(s, nil, false)
		ops, _ := s.GetUser("ops")
		own := a.IssueSession(ops)
		id, err := a.Authenticate(newReq(own).r)
		if err != nil || id.Tenant != perm.OperatorTenant || id.Acting || id.Perms[perm.Operator] != perm.Write {
			t.Fatalf("the operator's own session: %+v %v", id, err)
		}
		// going into a tenant, read-only
		tok, until := a.IssueActing(ops, "acme", time.Hour, false)
		id, err = a.Authenticate(newReq(tok).r)
		if err != nil || id.Tenant != "acme" || !id.Acting || id.Operator != "ops" || id.User != "ops" || id.ActingWrite || id.Perms[perm.Operator] != perm.None || id.Perms[perm.Users] != perm.Read || id.Perms[perm.Hosts] != perm.Read || !id.ActingUntil.Equal(time.Unix(until.Unix(), 0)) {
			t.Fatalf("inside the tenant: %+v", id)
		}
		for k, v := range id.Perms {
			if v == perm.Write {
				t.Fatalf("read-only means no write anywhere, but %s is write", k)
			}
		}
		// with write access on request, still without the console
		tok, _ = a.IssueActing(ops, "acme", time.Hour, true)
		id, _ = a.Authenticate(newReq(tok).r)
		if !id.ActingWrite || id.Perms[perm.Users] != perm.Write || id.Perms[perm.Operator] != perm.None {
			t.Fatalf("%+v", id)
		}
		// the time is capped
		tok, until = a.IssueActing(ops, "acme", 48*time.Hour, false)
		if until.After(time.Now().Add(MaxActing + time.Minute)) {
			t.Fatalf("an operator stays inside a tenant at most %v: %v", MaxActing, until)
		}
		// someone who is not an operator cannot act, even with a cookie that claims it
		boss, _ := s.GetUser("boss")
		fake, _ := a.IssueActing(boss, "globex", time.Hour, true)
		if id, err := a.Authenticate(newReq(fake).r); err != nil || id.Tenant != "acme" || id.Acting || id.Operator != "" {
			t.Fatalf("a customer's user cannot go into another tenant: %+v %v", id, err)
		}
		// an operator who loses the console is no longer let in
		g, _ := s.CreateGroup(perm.OperatorTenant, "Nothing", "", map[string]string{"dashboards": "read"})
		s.SetGroup("ops", g.ID)
		if id, _ := a.Authenticate(newReq(tok).r); id.Acting {
			t.Fatalf("losing the operator console ends the access: %+v", id)
		}
		s.SetGroup("ops", "admin")
		// when the time is up the same cookie is the operator's own again
		tok, _ = a.IssueActing(ops, "acme", time.Hour, false)
		msg, _, _ := strings.Cut(tok, ".")
		_ = msg
		short, _ := a.IssueActing(ops, "acme", time.Nanosecond, false)
		time.Sleep(1100 * time.Millisecond)
		if id, err := a.Authenticate(newReq(short).r); err != nil || id.Acting || id.Tenant != perm.OperatorTenant {
			t.Fatalf("after the time is up: %+v %v", id, err)
		}
		// the cookie cannot be edited to point at another tenant
		tok, _ = a.IssueActing(ops, "acme", time.Hour, false)
		m, sig, _ := strings.Cut(tok, ".")
		if _, err := a.Authenticate(newReq(m + "A." + sig).r); err == nil {
			t.Fatal("an edited cookie must fail")
		}
	})
}
