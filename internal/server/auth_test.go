package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/docstore/estest"
	"github.com/danielingemar/lumen/internal/model"
)

func init() { auth.Iterations = 1000 }

func authServer(t *testing.T) (*httptest.Server, *fakeStore, *auth.Auth) {
	return authServerOn(t, "file")
}

// authServerOn builds a server whose users/keys live in the given backend ("file" or "es" = fake Elasticsearch).
func authServerOn(t *testing.T, backend string) (*httptest.Server, *fakeStore, *auth.Auth) {
	var st *auth.Store
	var err error
	if backend == "es" {
		es, _ := estest.New()
		t.Cleanup(es.Close)
		b := docstore.NewES(docstore.ESConfig{URL: es.URL, Prefix: "t"})
		if err = b.EnsureIndices(context.Background()); err != nil {
			t.Fatal(err)
		}
		st, err = auth.Open(b, 5*time.Second)
	} else {
		st, err = auth.OpenDir(t.TempDir())
	}
	if err != nil {
		t.Fatal(err)
	}
	st.CreateUser("alice", "acme", "alices-long-password")
	st.CreateUser("bob", "globex", "bobs-long-password")
	a := auth.New(st, nil, false)
	fs := &fakeStore{}
	ts := httptest.NewServer(New(fs, a, slog.New(slog.NewTextHandler(io.Discard, nil))).WithAuth(a).Handler())
	t.Cleanup(ts.Close)
	return ts, fs, a
}

func client() *http.Client { j, _ := cookiejar.New(nil); return &http.Client{Jar: j} }

func post(c *http.Client, url, body string, hdr ...string) *http.Response {
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}

func get(c *http.Client, url string, hdr ...string) *http.Response {
	req, _ := http.NewRequest("GET", url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}

func login(t *testing.T, c *http.Client, ts *httptest.Server, user, pw string) int {
	b, _ := json.Marshal(map[string]string{"username": user, "password": pw})
	return post(c, ts.URL+"/api/v1/login", string(b)).StatusCode
}

func TestLoginFlow(t *testing.T) {
	ts, _, _ := authServer(t)
	c := client()
	if get(c, ts.URL+"/api/v1/me").StatusCode != 401 {
		t.Fatal("me without login must be 401")
	}
	if login(t, c, ts, "alice", "wrong-wrong-wrong") != 401 {
		t.Fatal("wrong password must be 401")
	}
	resp := post(c, ts.URL+"/api/v1/login", `{"username":"Alice","password":"alices-long-password"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("login failed: %d", resp.StatusCode)
	}
	ck := resp.Header.Get("Set-Cookie")
	if !strings.Contains(ck, "HttpOnly") || !strings.Contains(ck, "SameSite=Strict") || strings.Contains(strings.ToLower(ck), "alice") {
		t.Fatalf("cookie must be HttpOnly + SameSite=Strict and not expose the username in clear: %s", ck)
	}
	var me map[string]any
	json.NewDecoder(get(c, ts.URL+"/api/v1/me").Body).Decode(&me)
	if me["user"] != "alice" || me["tenant"] != "acme" || me["session"] != true {
		t.Fatalf("me: %+v", me)
	}
	post(c, ts.URL+"/api/v1/logout", `{}`)
	if get(c, ts.URL+"/api/v1/me").StatusCode != 401 {
		t.Fatal("logout must end the session")
	}
	// https (behind a proxy) must mark the cookie Secure
	r := post(client(), ts.URL+"/api/v1/login", `{"username":"alice","password":"alices-long-password"}`, "X-Forwarded-Proto", "https")
	if !strings.Contains(r.Header.Get("Set-Cookie"), "Secure") {
		t.Fatal("cookie must be Secure over https")
	}
}

func TestLoginRejectsNonJSONAndBrutePasswordsThrottled(t *testing.T) {
	ts, _, a := authServer(t)
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/login", strings.NewReader("username=alice&password=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded") // what a cross-site HTML form would send
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 415 {
		t.Fatalf("form posts must be refused (CSRF), got %d", resp.StatusCode)
	}
	a.Limiter = auth.NewLimiter(3, time.Minute)
	c := client()
	for i := 0; i < 3; i++ {
		login(t, c, ts, "alice", "wrong-wrong-wrong")
	}
	if code := login(t, c, ts, "alice", "alices-long-password"); code != 429 {
		t.Fatalf("after repeated failures even the right password is throttled, got %d", code)
	}
	if code := login(t, client(), ts, "bob", "bobs-long-password"); code != 200 {
		t.Fatal("other users are not affected")
	}
}

func TestSessionsReadButCannotIngest_KeysIngest(t *testing.T) {
	ts, fs, _ := authServer(t)
	c := client()
	login(t, c, ts, "alice", "alices-long-password")
	if get(c, ts.URL+"/api/v1/traces").StatusCode != 200 || fs.tenant != "acme" {
		t.Fatalf("session must read its own tenant, store saw %q", fs.tenant)
	}
	if resp := post(c, ts.URL+"/v1/traces", tracesJSON); resp.StatusCode != 403 {
		t.Fatalf("a browser session must not ingest telemetry, got %d", resp.StatusCode)
	}
	resp := post(c, ts.URL+"/api/v1/keys", `{"name":"vm1"}`)
	var k struct{ Key, ID string }
	json.NewDecoder(resp.Body).Decode(&k)
	if resp.StatusCode != 200 || !strings.HasPrefix(k.Key, "lmn_") {
		t.Fatalf("key creation: %d %+v", resp.StatusCode, k)
	}
	fs.spans = nil
	if r := post(http.DefaultClient, ts.URL+"/v1/traces", tracesJSON, "Authorization", "Bearer "+k.Key); r.StatusCode != 200 || len(fs.spans) != 1 || fs.spans[0].Tenant != "acme" {
		t.Fatalf("stored key must ingest for its tenant: %d %+v", r.StatusCode, fs.spans)
	}
	// an agent key can neither mint keys nor change passwords
	if r := post(http.DefaultClient, ts.URL+"/api/v1/keys", `{"name":"x"}`, "Authorization", "Bearer "+k.Key); r.StatusCode != 401 {
		t.Fatalf("API keys must not create keys, got %d", r.StatusCode)
	}
	// list shows prefix, never the key or its hash
	var list struct{ Data []map[string]any }
	body, _ := io.ReadAll(get(c, ts.URL+"/api/v1/keys").Body)
	json.Unmarshal(body, &list)
	if len(list.Data) != 1 || strings.Contains(string(body), k.Key) || strings.Contains(string(body), `"hash"`) {
		t.Fatalf("key list leaks secrets: %s", body)
	}
	// bob (other tenant) cannot see or delete alice's key
	cb := client()
	login(t, cb, ts, "bob", "bobs-long-password")
	body, _ = io.ReadAll(get(cb, ts.URL+"/api/v1/keys").Body)
	if !bytes.Contains(body, []byte(`"data":[]`)) {
		t.Fatalf("bob must see no keys: %s", body)
	}
	req, _ := http.NewRequest("DELETE", ts.URL+"/api/v1/keys/"+k.ID, nil)
	if resp, _ := cb.Do(req); resp.StatusCode != 404 {
		t.Fatalf("cross-tenant delete must 404, got %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("DELETE", ts.URL+"/api/v1/keys/"+k.ID, nil)
	if resp, _ := c.Do(req); resp.StatusCode != 200 {
		t.Fatalf("owner delete failed: %d", resp.StatusCode)
	}
	if r := post(http.DefaultClient, ts.URL+"/v1/traces", tracesJSON, "Authorization", "Bearer "+k.Key); r.StatusCode != 401 {
		t.Fatalf("deleted key must stop working, got %d", r.StatusCode)
	}
}

func TestChangePassword(t *testing.T) {
	ts, _, _ := authServer(t)
	c := client()
	login(t, c, ts, "alice", "alices-long-password")
	if post(c, ts.URL+"/api/v1/password", `{"current":"nope-nope-nope","new":"a-brand-new-password"}`).StatusCode != 401 {
		t.Fatal("wrong current password must be refused")
	}
	if post(c, ts.URL+"/api/v1/password", `{"current":"alices-long-password","new":"short"}`).StatusCode != 400 {
		t.Fatal("weak new password must be refused")
	}
	if post(c, ts.URL+"/api/v1/password", `{"current":"alices-long-password","new":"a-brand-new-password"}`).StatusCode != 200 {
		t.Fatal("password change failed")
	}
	if get(c, ts.URL+"/api/v1/me").StatusCode != 401 {
		t.Fatal("changing the password must end the session")
	}
	if login(t, client(), ts, "alice", "alices-long-password") != 401 || login(t, client(), ts, "alice", "a-brand-new-password") != 200 {
		t.Fatal("old password must stop working, new must work")
	}
}

func TestLoginWithAPIKey(t *testing.T) {
	for _, backend := range []string{"file", "es"} {
		t.Run(backend, func(t *testing.T) {
			ts, fs, a := authServerOn(t, backend)
			plain, _, _ := a.Store.CreateKey("acme", "vm1")
			c := client()
			if post(c, ts.URL+"/api/v1/login", `{"api_key":"lmn_wrong"}`).StatusCode != 401 {
				t.Fatal("a wrong API key must be refused")
			}
			resp := post(c, ts.URL+"/api/v1/login", `{"api_key":"`+plain+`"}`)
			var out map[string]any
			json.NewDecoder(resp.Body).Decode(&out)
			if resp.StatusCode != 200 || out["tenant"] != "acme" || out["via_key"] != true {
				t.Fatalf("api key login: %d %+v", resp.StatusCode, out)
			}
			if get(c, ts.URL+"/api/v1/traces").StatusCode != 200 || fs.tenant != "acme" {
				t.Fatalf("key session must read its tenant, store saw %q", fs.tenant)
			}
			if r := post(c, ts.URL+"/api/v1/keys", `{"name":"x"}`); r.StatusCode != 403 {
				t.Fatalf("a key session must not create keys, got %d", r.StatusCode)
			}
			if r := post(c, ts.URL+"/api/v1/password", `{"current":"a","new":"b"}`); r.StatusCode != 403 {
				t.Fatalf("a key session must not change passwords, got %d", r.StatusCode)
			}
			if r := post(c, ts.URL+"/v1/traces", tracesJSON); r.StatusCode != 403 {
				t.Fatalf("a browser session of any kind must not ingest, got %d", r.StatusCode)
			}
			var me map[string]any
			json.NewDecoder(get(c, ts.URL+"/api/v1/me").Body).Decode(&me)
			if me["via_key"] != true || me["user"] != "" {
				t.Fatalf("me: %+v", me)
			}
			a.Store.DeleteKey("acme", a.Store.ListKeys("acme")[0].ID)
			if get(c, ts.URL+"/api/v1/me").StatusCode != 401 {
				t.Fatal("deleting the key must end the browser session")
			}
		})
	}
}

func TestLoginFlowOnElasticsearchBackend(t *testing.T) {
	ts, _, _ := authServerOn(t, "es")
	c := client()
	if login(t, c, ts, "alice", "alices-long-password") != 200 {
		t.Fatal("login must work with users stored in Elasticsearch")
	}
	if get(c, ts.URL+"/api/v1/me").StatusCode != 200 {
		t.Fatal("session must work")
	}
}

func TestSeriesEndpoint(t *testing.T) {
	ts, fs, _ := authServer(t)
	c := client()
	login(t, c, ts, "alice", "alices-long-password")
	resp := get(c, ts.URL+"/api/v1/series?source=metric&name=cpu&agg=avg&group_by=host&filter=host:h1&filter=env:prod&from=2026-01-01T00:00:00Z&to=2026-01-01T06:00:00Z")
	var out struct {
		Data []model.Series
		Step int
	}
	json.NewDecoder(resp.Body).Decode(&out)
	q := fs.lastSeries
	if resp.StatusCode != 200 || len(out.Data) != 1 || out.Step != 300 { // 6h / 120 points = 180s -> next nice step 300s
		t.Fatalf("series: %d %+v", resp.StatusCode, out)
	}
	if fs.tenant != "acme" || q.Source != "metric" || q.Name != "cpu" || q.GroupBy != "host" || q.Filters["host"] != "h1" || q.Filters["env"] != "prod" {
		t.Fatalf("query not passed through (tenant must come from the login): %+v tenant=%s", q, fs.tenant)
	}
	for _, bad := range []string{
		"source=bad", "source=metric&name=x&filter=nocolon", "source=metric&name=x&from=2026-01-02T00:00:00Z&to=2026-01-01T00:00:00Z",
		"source=metric&name=x&from=2020-01-01T00:00:00Z&to=2026-01-01T00:00:00Z", // > 90 days
		"source=metric&name=x&filter=a:1&filter=b:2&filter=c:3&filter=d:4&filter=e:5&filter=f:6",
	} {
		if r := get(c, ts.URL+"/api/v1/series?"+bad); r.StatusCode != 400 {
			t.Errorf("%s must be 400, got %d", bad, r.StatusCode)
		}
	}
	if r := get(http.DefaultClient, ts.URL+"/api/v1/series?source=logs"); r.StatusCode != 401 {
		t.Fatal("series needs authentication")
	}
	if r := get(c, ts.URL+"/api/v1/metrics/labels"); r.StatusCode != 400 {
		t.Fatal("labels needs a metric name")
	}
	for _, p := range []string{"/api/v1/services", "/api/v1/metrics/names"} {
		if r := get(c, ts.URL+p); r.StatusCode != 200 {
			t.Fatalf("%s: %d", p, r.StatusCode)
		}
	}
}

func TestPickStep(t *testing.T) {
	cases := []struct {
		rng       time.Duration
		req, want int
	}{
		{time.Hour, 0, 30}, {15 * time.Minute, 0, 10}, {24 * time.Hour, 0, 900}, {7 * 24 * time.Hour, 0, 7200},
		{time.Hour, 5, 5}, {24 * time.Hour, 10, 87}, // a requested step is raised so the chart has at most ~1000 points
	}
	for _, c := range cases {
		if got := pickStep(c.rng, c.req); got != c.want {
			t.Errorf("pickStep(%v,%d)=%d want %d", c.rng, c.req, got, c.want)
		}
	}
}
