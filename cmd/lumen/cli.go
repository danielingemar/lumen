package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/config"
)

const usage = `Manage Lumen users and API keys. They are stored in Elasticsearch when LUMEN_ELASTICSEARCH_URL is set, otherwise in $LUMEN_DATA_DIR (default /data).

  lumen users list
  lumen users add NAME --tenant TENANT [--group admin|user|GROUP_ID] [--password-stdin]   (default group: user = read-only; no password given: a random one is generated and printed once)
  lumen users set-group NAME GROUP
  lumen users passwd NAME [--password-stdin]
  lumen users delete NAME
  lumen keys list --tenant TENANT
  lumen keys create --tenant TENANT [--name NAME]           (the key is printed once and cannot be shown again)
  lumen keys delete --tenant TENANT ID

In Docker:  docker compose -f deploy/docker-compose.yml exec lumen /lumen users add alice --tenant main
`

func flag(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func has(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func password(args []string) (pw string, generated bool, err error) {
	if has(args, "--password-stdin") {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", false, fmt.Errorf("no password on stdin")
		}
		return strings.TrimRight(line, "\r\n"), false, nil
	}
	return auth.RandomToken(9), true, nil // 18 hex characters
}

func runCLI(args []string) int {
	if len(args) < 2 || args[1] == "-h" || args[1] == "--help" {
		fmt.Print(usage)
		return 2
	}
	store, _, err := openStore(config.Load(), nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fail := func(err error) int { fmt.Fprintln(os.Stderr, "error:", err); return 1 }
	rest := args[2:]
	switch args[0] + " " + args[1] {
	case "users list":
		for _, u := range store.ListUsers() {
			fmt.Printf("%-24s tenant=%s group=%s created=%s\n", u.Name, u.Tenant, auth.EffectiveGroup(u), u.Created.Format("2006-01-02"))
		}
	case "users add":
		if len(rest) < 1 {
			fmt.Print(usage)
			return 2
		}
		tenant, ok := flag(rest, "--tenant")
		if !ok {
			return fail(fmt.Errorf("--tenant is required"))
		}
		pw, gen, err := password(rest)
		if err != nil {
			return fail(err)
		}
		group, _ := flag(rest, "--group")
		if group == "" {
			group = "user"
		}
		if err := store.CreateUserIn(rest[0], tenant, pw, group); err != nil {
			return fail(err)
		}
		fmt.Printf("user %q created in tenant %q, group %q\n", strings.ToLower(rest[0]), tenant, group)
		if gen {
			fmt.Printf("password (shown once): %s\n", pw)
		}
	case "users set-group":
		if len(rest) < 2 {
			fmt.Print(usage)
			return 2
		}
		if err := store.SetGroup(rest[0], rest[1]); err != nil {
			return fail(err)
		}
		fmt.Println("group changed")
	case "users passwd":
		if len(rest) < 1 {
			fmt.Print(usage)
			return 2
		}
		pw, gen, err := password(rest)
		if err != nil {
			return fail(err)
		}
		if err := store.SetPassword(rest[0], pw); err != nil {
			return fail(err)
		}
		fmt.Println("password changed; existing logins for this user are ended")
		if gen {
			fmt.Printf("new password (shown once): %s\n", pw)
		}
	case "users delete":
		if len(rest) < 1 {
			fmt.Print(usage)
			return 2
		}
		if err := store.DeleteUser(rest[0]); err != nil {
			return fail(err)
		}
		fmt.Println("user deleted")
	case "keys list":
		tenant, ok := flag(rest, "--tenant")
		if !ok {
			return fail(fmt.Errorf("--tenant is required"))
		}
		for _, k := range store.ListKeys(tenant) {
			fmt.Printf("%s  %s...  %-20s created=%s\n", k.ID, k.Prefix, k.Name, k.Created.Format("2006-01-02"))
		}
	case "keys create":
		tenant, ok := flag(rest, "--tenant")
		if !ok {
			return fail(fmt.Errorf("--tenant is required"))
		}
		name, _ := flag(rest, "--name")
		plain, k, err := store.CreateKey(tenant, name)
		if err != nil {
			return fail(err)
		}
		fmt.Printf("key id %s for tenant %q. The key (shown once):\n%s\n", k.ID, tenant, plain)
	case "keys delete":
		tenant, ok := flag(rest, "--tenant")
		if !ok || len(rest) < 3 {
			return fail(fmt.Errorf("usage: lumen keys delete --tenant TENANT ID"))
		}
		id := rest[len(rest)-1]
		if err := store.DeleteKey(tenant, id); err != nil {
			return fail(err)
		}
		fmt.Println("key deleted")
	default:
		fmt.Print(usage)
		return 2
	}
	return 0
}
