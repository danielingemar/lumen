package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/license"
)

func runTool(t *testing.T, args ...string) (string, string, int) {
	var out, errw bytes.Buffer
	code := run(args, &out, &errw)
	return out.String(), errw.String(), code
}

func TestToolEndToEnd(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	out, _, code := runTool(t, "keygen", "--id", "main", "--dir", keys)
	if code != 0 || !strings.Contains(out, "SECRET") || !strings.Contains(out, "internal/license/keys/main.pub") {
		t.Fatalf("%d %s", code, out)
	}
	if st, _ := os.Stat(filepath.Join(keys, "main.key")); st.Mode().Perm() != 0o600 {
		t.Fatalf("the private key must be readable by its owner only: %v", st.Mode())
	}
	// a key is never overwritten
	if _, e, code := runTool(t, "keygen", "--id", "main", "--dir", keys); code != 1 || !strings.Contains(e, "never overwritten") {
		t.Fatalf("%d %s", code, e)
	}
	if _, _, code := runTool(t, "keygen", "--dir", keys); code != 1 {
		t.Fatal("a key needs a name")
	}
	lic, ledger := filepath.Join(dir, "acme.license"), filepath.Join(dir, "issued.jsonl")
	out, e, code := runTool(t, "issue", "--key", filepath.Join(keys, "main.key"), "--customer", "ACME AB", "--editions", "enterprise,operator", "--days", "365", "--hosts", "50", "--tenants", "10", "--out", lic, "--ledger", ledger)
	if code != 0 || !strings.Contains(out, "ACME AB") || !strings.Contains(out, "enterprise+operator") || !strings.Contains(out, "hosts limit 50") {
		t.Fatalf("%d %s %s", code, out, e)
	}
	// the file verifies with the public key the tool made, and says what was asked for
	pubText, _ := os.ReadFile(filepath.Join(keys, "main.pub"))
	pub, err := license.ParsePublicKey(string(pubText))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(lic)
	l, err := license.Parse(raw, license.Keys{"main": pub})
	if err != nil || l.Customer != "ACME AB" || !l.Has(license.Operator) || l.Limits.Hosts != 50 || l.Limits.Tenants != 10 || l.KeyID != "main" || !strings.HasPrefix(l.ID, "L-") {
		t.Fatalf("%+v %v", l, err)
	}
	if d := l.DaysLeft(time.Now()); d < 364 || d > 365 {
		t.Fatalf("365 days: %d", d)
	}
	// the ledger is your own record
	b, _ := os.ReadFile(ledger)
	var rec map[string]any
	if json.Unmarshal(bytes.TrimSpace(b), &rec) != nil || rec["customer"] != "ACME AB" || rec["id"] != l.ID {
		t.Fatalf("ledger: %s", b)
	}
	// inspect, with and without verification, with the file name before or after the flag
	out, _, code = runTool(t, "inspect", lic, "--pub", filepath.Join(keys, "main.pub"))
	if code != 0 || !strings.Contains(out, "signature OK") || !strings.Contains(out, "ACME AB") || !strings.Contains(out, "valid") {
		t.Fatalf("%d %s", code, out)
	}
	out, _, _ = runTool(t, "inspect", lic)
	if !strings.Contains(out, "not verified") {
		t.Fatalf("%s", out)
	}
	// another publisher's key does not verify it
	runTool(t, "keygen", "--id", "other", "--dir", keys)
	if _, e, code := runTool(t, "inspect", lic, "--pub", filepath.Join(keys, "other.pub")); code != 1 || !strings.Contains(e, "signature does not match") {
		t.Fatalf("%d %s", code, e)
	}
	// a fixed end date, and refusing nonsense
	out, _, code = runTool(t, "issue", "--key", filepath.Join(keys, "main.key"), "--customer", "Trial AB", "--expires", "2099-01-31", "--out", filepath.Join(dir, "trial.license"))
	if code != 0 || !strings.Contains(out, "valid until 2099-01-31") {
		t.Fatalf("%d %s", code, out)
	}
	for name, args := range map[string][]string{
		"neither days nor date": {"--key", filepath.Join(keys, "main.key"), "--customer", "x"}, "both": {"--key", filepath.Join(keys, "main.key"), "--customer", "x", "--days", "5", "--expires", "2099-01-01"},
		"bad date": {"--key", filepath.Join(keys, "main.key"), "--customer", "x", "--expires", "tomorrow"}, "no customer": {"--key", filepath.Join(keys, "main.key"), "--days", "5"},
		"bad edition": {"--key", filepath.Join(keys, "main.key"), "--customer", "x", "--days", "5", "--editions", "gold"}, "no key": {"--customer", "x", "--days", "5"},
		"public key as private": {"--key", filepath.Join(keys, "main.pub"), "--customer", "x", "--days", "5"},
	} {
		if _, _, code := runTool(t, append([]string{"issue", "--out", filepath.Join(dir, "x.license")}, args...)...); code != 1 {
			t.Errorf("%s must fail", name)
		}
	}
	// a licence can be dated earlier: it is then already over, which is how the tests of the grace period are made
	out, _, code = runTool(t, "issue", "--key", filepath.Join(keys, "main.key"), "--customer", "Old AB", "--issued", "2024-01-01", "--expires", "2024-12-31", "--out", filepath.Join(dir, "old.license"))
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	rawOld, _ := os.ReadFile(filepath.Join(dir, "old.license"))
	lo, _ := license.Parse(rawOld, license.Keys{"main": pub})
	if lo.State(time.Now()) != license.StateExpired || lo.Issued.Year() != 2024 {
		t.Fatalf("%+v", lo)
	}
	if _, _, code := runTool(t, "issue", "--key", filepath.Join(keys, "main.key"), "--customer", "x", "--issued", "yesterday", "--days", "5", "--out", filepath.Join(dir, "x.license")); code != 1 {
		t.Fatal("bad issue date")
	}
	if _, _, code := runTool(t); code != 2 {
		t.Fatal("usage")
	}
	if _, _, code := runTool(t, "frobnicate"); code != 2 {
		t.Fatal("unknown command")
	}
}
