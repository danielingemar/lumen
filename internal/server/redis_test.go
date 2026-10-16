package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/inmagma/lumen/internal/model"
)

func TestRedisPasswordsStayOnTheServerAndReachOnlyTheRightAgent(t *testing.T) {
	ts, a, fs := regServer(t)
	ad, rd, ot := client(), client(), client()
	login(t, ad, ts, "admin", "admins-long-password")
	login(t, rd, ts, "reader", "readers-long-password")
	login(t, ot, ts, "other", "others-long-password")
	now := time.Now().Unix()
	at := func(redis string, extra map[string]string) map[string]string {
		m := map[string]string{"host": "web1", "redis": redis}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	fs.latestFor = "acme"
	fs.latest = []model.Latest{
		{Name: "lumen_agent_info", Attrs: map[string]string{"host": "web1", "version": "0.3", "os": "linux/amd64"}, Value: 1, T: now - 3},
		{Name: "redis_up", Attrs: at("cache", nil), Value: 1, T: now - 3},
		{Name: "redis_info", Attrs: at("cache", map[string]string{"container": "cache", "version": "7.2.4", "role": "master"}), Value: 1, T: now - 3},
		{Name: "redis_memory_used_bytes", Attrs: at("cache", nil), Value: 1000, T: now - 3},
		{Name: "redis_up", Attrs: at("sessions", nil), Value: 0, T: now - 3},
		{Name: "redis_auth_required", Attrs: at("sessions", nil), Value: 1, T: now - 3},
		{Name: "redis_info", Attrs: at("sessions", map[string]string{"container": "sessions"}), Value: 1, T: now - 3},
	}
	_, b := do(rd, "GET", ts.URL+"/api/v1/redis", "")
	var list struct {
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(b, &list)
	if len(list.Data) != 2 || list.Data[0]["name"] != "sessions" || list.Data[0]["state"] != "auth" || list.Data[1]["state"] != "up" || list.Data[1]["memory_used"] != float64(1000) {
		t.Fatalf("list: %s (a Redis that wants a password is listed first, as auth)", b)
	}
	if code(rd, "POST", ts.URL+"/api/v1/redis", `{"host":"web1","container":"sessions","password":"x"}`) != 403 {
		t.Fatal("a read-only user must not set passwords")
	}
	r, b := do(ad, "POST", ts.URL+"/api/v1/redis", `{"host":"web1","container":"sessions","password":"REDIS-SECRET"}`)
	var out map[string]any
	json.Unmarshal(b, &out)
	if r.StatusCode != 200 || out["has_password"] != true || strings.Contains(string(b), "REDIS-SECRET") {
		t.Fatalf("create: %d %s", r.StatusCode, b)
	}
	id := out["id"].(string)
	if code(ad, "POST", ts.URL+"/api/v1/redis", `{"host":"web1","container":"sessions","password":"y"}`) != 400 {
		t.Fatal("the same Redis twice")
	}
	if code(ad, "POST", ts.URL+"/api/v1/redis", `{"host":"web1","addr":"nope"}`) != 400 || code(ad, "POST", ts.URL+"/api/v1/redis", `{"host":"web1"}`) != 400 {
		t.Fatal("an address needs host:port, and something must identify the Redis")
	}
	_, b = do(rd, "GET", ts.URL+"/api/v1/redis", "")
	if strings.Contains(string(b), "REDIS-SECRET") || !strings.Contains(string(b), `"has_password":true`) {
		t.Fatalf("list after: %s", b)
	}
	plain, _, _ := a.Store.CreateKey("acme", "web1")
	if resp, body := get2(t, ts.URL, plain, "/api/v1/agent/config?host=web1"); resp != 200 || !strings.Contains(body, "REDIS-SECRET") || !strings.Contains(body, `"container":"sessions"`) {
		t.Fatalf("agent config: %d %s", resp, body)
	}
	if _, body := get2(t, ts.URL, plain, "/api/v1/agent/config?host=web2"); strings.Contains(body, "REDIS-SECRET") {
		t.Fatalf("another host must not get it: %s", body)
	}
	if code(ot, "PUT", ts.URL+"/api/v1/redis/"+id, `{"host":"web1","container":"sessions","password":"evil"}`) != 404 || code(ot, "DELETE", ts.URL+"/api/v1/redis/"+id, "") != 404 {
		t.Fatal("another tenant must not touch it")
	}
	// a Redis without a stored password keeps it when edited without one; it can be cleared
	if code(ad, "PUT", ts.URL+"/api/v1/redis/"+id, `{"host":"web1","container":"sessions","username":"mon"}`) != 200 {
		t.Fatal("update")
	}
	if _, body := get2(t, ts.URL, plain, "/api/v1/agent/config?host=web1"); !strings.Contains(body, "REDIS-SECRET") || !strings.Contains(body, `"username":"mon"`) {
		t.Fatalf("editing without a password keeps the old one: %s", body)
	}
	code(ad, "PUT", ts.URL+"/api/v1/redis/"+id, `{"host":"web1","container":"sessions","clear_password":true}`)
	if _, body := get2(t, ts.URL, plain, "/api/v1/agent/config?host=web1"); strings.Contains(body, "REDIS-SECRET") {
		t.Fatalf("cleared: %s", body)
	}
	if code(ad, "DELETE", ts.URL+"/api/v1/redis/"+id, "") != 200 {
		t.Fatal("delete")
	}
}

func get2(t *testing.T, base, key, p string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+p, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	bb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(bb)
}
