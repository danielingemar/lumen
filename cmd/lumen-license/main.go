// Command lumen-license is the publisher's tool: it makes the signing key pair and issues licence files.
//
// Keep the private key (*.key) secret and off any server that customers can reach. It is the only thing that can make
// a licence. The public key (*.pub) is not secret: it goes into internal/license/keys/ and is built into the program.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/danielingemar/lumen/internal/license"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

const usage = `lumen-license: make signing keys and issue licences

  lumen-license keygen  --id main [--dir keys]
  lumen-license issue   --key keys/main.key --key-id main --customer "ACME AB"
                        --editions enterprise[,operator] (--days 365 | --expires 2027-12-31) [--issued 2026-01-31]
                        [--hosts 50] [--tenants 10] [--id L-2026-001] [--out acme.license] [--ledger issued.jsonl]
  lumen-license inspect FILE [--pub keys/main.pub]
`

func run(args []string, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errw, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "keygen":
		err = keygen(args[1:], out)
	case "issue":
		err = issue(args[1:], out)
	case "inspect":
		err = inspect(args[1:], out)
	case "-h", "--help", "help":
		fmt.Fprint(out, usage)
		return 0
	default:
		fmt.Fprintf(errw, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(errw, "error:", err)
		return 1
	}
	return 0
}

var idRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,39}$`)

func keygen(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	id := fs.String("id", "", "name of the key, for example main (it is written into every licence)")
	dir := fs.String("dir", "keys", "directory for the key files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !idRe.MatchString(*id) {
		return fmt.Errorf("give the key a short name with --id, for example --id main")
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	priv, pub := filepath.Join(*dir, *id+".key"), filepath.Join(*dir, *id+".pub")
	for _, p := range []string{priv, pub} {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("%s already exists; a key is never overwritten (that would make every licence you issued unverifiable)", p)
		}
	}
	pk, sk, err := license.NewKeyPair()
	if err != nil {
		return err
	}
	if err := os.WriteFile(priv, []byte(license.EncodeKey(sk)+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(pub, []byte(license.EncodeKey(pk)+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, `Key pair "%s" made.

  private key  %s   SECRET. Back it up offline. Never commit it, never put it on a server customers can reach.
  public key   %s   not secret

Next:
  1. Copy the public key into the program:  cp %s internal/license/keys/%s.pub   and commit it.
  2. Build the program (the key is built in).
  3. Issue a licence:  lumen-license issue --key %s --key-id %s --customer "..." --editions enterprise --days 365
`, *id, priv, pub, pub, *id, priv, *id)
	return nil
}

func issue(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("issue", flag.ContinueOnError)
	keyFile := fs.String("key", "", "the private key file")
	keyID := fs.String("key-id", "", "the name of the key (default: the file name without .key)")
	customer := fs.String("customer", "", "the customer's name")
	editions := fs.String("editions", "enterprise", "comma-separated: enterprise, operator")
	days := fs.Int("days", 0, "valid for this many days from today")
	expires := fs.String("expires", "", "valid until this date (YYYY-MM-DD, end of that day UTC)")
	issued := fs.String("issued", "", "date of issue (YYYY-MM-DD, default today): a renewal can start when the old licence ended")
	hosts := fs.Int("hosts", 0, "soft limit of hosts (0 = none)")
	tenants := fs.Int("tenants", 0, "soft limit of tenants (0 = none)")
	id := fs.String("id", "", "licence id (default: made from the date and a random part)")
	outFile := fs.String("out", "", "file to write (default: <customer>-<id>.license)")
	ledger := fs.String("ledger", "", "append a line about the licence to this file (your own record of what you issued)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keyFile == "" {
		return fmt.Errorf("--key is required")
	}
	raw, err := os.ReadFile(*keyFile)
	if err != nil {
		return err
	}
	if st, err := os.Stat(*keyFile); err == nil && st.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(out, "warning: %s can be read by other users on this machine; run chmod 600 on it\n", *keyFile)
	}
	priv, err := license.ParsePrivateKey(string(raw))
	if err != nil {
		return err
	}
	if *keyID == "" {
		*keyID = strings.TrimSuffix(filepath.Base(*keyFile), ".key")
	}
	now := time.Now().UTC().Truncate(time.Second)
	if *issued != "" {
		d, err := time.Parse("2006-01-02", *issued)
		if err != nil {
			return fmt.Errorf("--issued must look like 2026-01-31")
		}
		now = d
	}
	var end time.Time
	switch {
	case *days > 0 && *expires == "":
		end = now.AddDate(0, 0, *days)
	case *expires != "" && *days == 0:
		d, err := time.Parse("2006-01-02", *expires)
		if err != nil {
			return fmt.Errorf("--expires must look like 2027-12-31")
		}
		end = d.Add(24*time.Hour - time.Second)
	default:
		return fmt.Errorf("give exactly one of --days and --expires")
	}
	if *id == "" {
		b := make([]byte, 3)
		_, _ = rand.Read(b)
		*id = "L-" + now.Format("20060102") + "-" + hex.EncodeToString(b)
	}
	p := license.Payload{ID: *id, Customer: strings.TrimSpace(*customer), Issued: now, Expires: end, Limits: license.Limits{Hosts: *hosts, Tenants: *tenants}}
	for _, e := range strings.Split(*editions, ",") {
		if e = strings.TrimSpace(e); e != "" {
			p.Editions = append(p.Editions, e)
		}
	}
	file, err := license.Sign(priv, *keyID, p)
	if err != nil {
		return err
	}
	if *outFile == "" {
		slug := strings.Trim(regexp.MustCompile(`[^a-zA-Z0-9]+`).ReplaceAllString(strings.ToLower(p.Customer), "-"), "-")
		*outFile = slug + "-" + *id + ".license"
	}
	if err := os.WriteFile(*outFile, file, 0o644); err != nil {
		return err
	}
	if *ledger != "" {
		rec, _ := json.Marshal(map[string]any{"id": p.ID, "customer": p.Customer, "editions": p.Editions, "issued": p.Issued, "expires": p.Expires, "limits": p.Limits, "key_id": *keyID, "file": *outFile})
		f, err := os.OpenFile(*ledger, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Write(append(rec, '\n')); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "Licence %s for %s written to %s\n  editions %s, valid until %s (%d days), hosts limit %s, tenants limit %s\n",
		p.ID, p.Customer, *outFile, strings.Join(p.Editions, "+"), p.Expires.Format("2006-01-02"), int(p.Expires.Sub(now).Hours()/24), limit(p.Limits.Hosts), limit(p.Limits.Tenants))
	return nil
}

func limit(n int) string {
	if n == 0 {
		return "none"
	}
	return fmt.Sprint(n)
}

func inspect(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	pubFile := fs.String("pub", "", "a public key file: if given, the signature is verified")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("give the licence file: lumen-license inspect FILE")
	}
	raw, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	var f license.File
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("that is not a licence file")
	}
	verified := "not verified (give --pub to check the signature)"
	var l license.License
	if *pubFile != "" {
		b, err := os.ReadFile(*pubFile)
		if err != nil {
			return err
		}
		pub, err := license.ParsePublicKey(string(b))
		if err != nil {
			return err
		}
		l, err = license.Parse(raw, license.Keys{f.KeyID: pub})
		if err != nil {
			return err
		}
		verified = "signature OK"
	} else {
		body, err := license.Unverified(raw)
		if err != nil {
			return err
		}
		l = license.License{Payload: body, KeyID: f.KeyID}
	}
	now := time.Now()
	fmt.Fprintf(out, "id        %s\ncustomer  %s\neditions  %s\nissued    %s\nexpires   %s (%s, %d days)\nhosts     %s\ntenants   %s\nkey       %s\n%s\n",
		l.ID, l.Customer, strings.Join(l.Editions, ", "), l.Issued.Format("2006-01-02"), l.Expires.Format("2006-01-02 15:04 UTC"), l.State(now), l.DaysLeft(now), limit(l.Limits.Hosts), limit(l.Limits.Tenants), f.KeyID, verified)
	return nil
}

// reorder puts flags before the file name, so "inspect FILE --pub k.pub" works too.
func reorder(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flags = append(flags, args[i])
			if !strings.Contains(args[i], "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		} else {
			rest = append(rest, args[i])
		}
	}
	return append(flags, rest...)
}
