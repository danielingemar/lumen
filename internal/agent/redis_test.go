package agent

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeRedis speaks enough of the protocol for AUTH and INFO. user "" and pass "" = no password at all.
func fakeRedis(t *testing.T, user, pass string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				authed := pass == ""
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					n, _ := strconv.Atoi(strings.TrimSpace(l)[1:])
					var args []string
					for i := 0; i < n; i++ {
						r.ReadString('\n')
						a, _ := r.ReadString('\n')
						args = append(args, strings.TrimRight(a, "\r\n"))
					}
					switch strings.ToUpper(args[0]) {
					case "AUTH":
						switch {
						case pass == "":
							fmt.Fprint(c, "-ERR AUTH <password> called without any password configured for the default user. Are you sure your configuration is correct?\r\n")
						case len(args) == 2 && user == "" && args[1] == pass, len(args) == 3 && args[1] == user && args[2] == pass:
							authed = true
							fmt.Fprint(c, "+OK\r\n")
						default:
							fmt.Fprint(c, "-WRONGPASS invalid username-password pair or user is disabled.\r\n")
						}
					case "INFO":
						if !authed {
							fmt.Fprint(c, "-NOAUTH Authentication required.\r\n")
							continue
						}
						body := "# Server\r\nredis_version:7.2.4\r\nredis_mode:standalone\r\nuptime_in_seconds:100\r\n# Clients\r\nconnected_clients:3\r\n# Memory\r\nused_memory:1048576\r\nmaxmemory:2097152\r\n# Replication\r\nrole:master\r\nconnected_slaves:1\r\n# Stats\r\nevicted_keys:7\r\nkeyspace_hits:90\r\nkeyspace_misses:10\r\n# Keyspace\r\ndb0:keys=5,expires=1,avg_ttl=0\r\ndb1:keys=2,expires=0,avg_ttl=0\r\n"
						fmt.Fprintf(c, "$%d\r\n%s\r\n", len(body), body)
					default:
						fmt.Fprint(c, "-ERR unknown\r\n")
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

func ctx5() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func TestRedisInfoWithAndWithoutPassword(t *testing.T) {
	c, cancel := ctx5()
	defer cancel()
	open := fakeRedis(t, "", "")
	locked := fakeRedis(t, "", "s3cret")
	acl := fakeRedis(t, "mon", "pw")

	if m, err := RedisInfo(c, open, "", ""); err != nil || m["redis_version"] != "7.2.4" {
		t.Fatalf("no password: %v %v", m, err)
	}
	if _, err := RedisInfo(c, open, "", "unneeded"); err != nil { // a password given to a Redis that has none is fine
		t.Fatalf("password not needed: %v", err)
	}
	if _, err := RedisInfo(c, locked, "", ""); err != ErrRedisAuth {
		t.Fatalf("missing password must say so, got %v", err)
	}
	if _, err := RedisInfo(c, locked, "", "wrong"); err != ErrRedisAuth {
		t.Fatalf("wrong password must say so, got %v", err)
	}
	if _, err := RedisInfo(c, locked, "", "s3cret"); err != nil {
		t.Fatalf("right password: %v", err)
	}
	if _, err := RedisInfo(c, acl, "mon", "pw"); err != nil {
		t.Fatalf("acl user: %v", err)
	}
	if _, err := RedisInfo(c, acl, "other", "pw"); err != ErrRedisAuth {
		t.Fatalf("wrong acl user: %v", err)
	}
	if _, err := RedisInfo(c, "127.0.0.1:1", "", ""); err == nil || err == ErrRedisAuth {
		t.Fatalf("closed port must be an ordinary error, got %v", err)
	}
}

func TestCollectRedisSeveralAtOnce(t *testing.T) {
	c, cancel := ctx5()
	defer cancel()
	open := fakeRedis(t, "", "")
	locked := fakeRedis(t, "", "s3cret")
	needs := fakeRedis(t, "", "other")
	pts, err := CollectRedis(c, "/nonexistent/docker.sock", []RedisTarget{
		{Name: "cache", Addr: open},
		{Name: "sessions", Addr: locked, Password: "s3cret"},
		{Name: "queue", Addr: needs},
	}, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	val := func(name, redis string) (float64, bool) {
		for _, p := range pts {
			if p.Name == name && p.Attrs["redis"] == redis {
				return p.Value, true
			}
		}
		return 0, false
	}
	for r, want := range map[string]float64{"cache": 1, "sessions": 1, "queue": 0} {
		if v, ok := val("redis_up", r); !ok || v != want {
			t.Errorf("redis_up %s = %v (found %v), want %v", r, v, ok, want)
		}
	}
	if v, _ := val("redis_auth_required", "queue"); v != 1 {
		t.Error("queue should be flagged as needing a password")
	}
	if v, _ := val("redis_auth_required", "cache"); v != 0 {
		t.Error("cache needs no password")
	}
	if v, _ := val("redis_memory_used_bytes", "sessions"); v != 1048576 {
		t.Errorf("memory = %v", v)
	}
	if v, _ := val("redis_keys", "cache"); v != 7 {
		t.Errorf("keys = %v, want 7 (all databases)", v)
	}
	if _, ok := val("redis_memory_used_bytes", "queue"); ok {
		t.Error("no values for a Redis we could not log in to")
	}
	// the shared default password reaches a Redis without its own
	pts, _ = CollectRedis(c, "/nonexistent/docker.sock", []RedisTarget{{Name: "queue", Addr: needs}}, "other", 1)
	if v, _ := val("redis_up", "queue"); v != 1 {
		t.Error("default password should have worked")
	}
}

func TestRedisImageRecognition(t *testing.T) {
	yes := []string{"redis", "redis:7-alpine", "docker.io/library/redis:6.2", "valkey/valkey:8", "eqalpha/keydb", "bitnami/redis:7.0", "redis/redis-stack-server"}
	no := []string{"oliver006/redis_exporter", "redislabs/redisinsight", "rediscommander/redis-commander", "nextcloud", "postgres:16", "bitnami/redis-sentinel"}
	for _, i := range yes {
		if !redisImage(i) {
			t.Errorf("%s should count as Redis", i)
		}
	}
	for _, i := range no {
		if redisImage(i) {
			t.Errorf("%s should not count as Redis", i)
		}
	}
}

func TestRedisAddressesFromContainers(t *testing.T) {
	var cs []dockerRedisContainer
	cs = append(cs, dockerRedisContainer{Names: []string{"/cache"}, Image: "redis:7", State: "running"})
	cs[0].NetworkSettings.Networks = map[string]struct{ IPAddress string }{"app": {"172.18.0.5"}}
	cs[0].Ports = append(cs[0].Ports, struct {
		PrivatePort int
		PublicPort  int
		IP          string
		Type        string
	}{6379, 6380, "0.0.0.0", "tcp"})
	cs = append(cs, dockerRedisContainer{Names: []string{"/off"}, Image: "redis", State: "exited"})
	cs = append(cs, dockerRedisContainer{Names: []string{"/ui"}, Image: "redislabs/redisinsight", State: "running"})
	got := redisFromContainers(cs)
	if len(got) != 1 || got[0].Container != "cache" || len(got[0].Addrs) != 2 || got[0].Addrs[0] != "172.18.0.5:6379" || got[0].Addrs[1] != "127.0.0.1:6380" {
		t.Fatalf("unexpected: %+v", got)
	}
}
