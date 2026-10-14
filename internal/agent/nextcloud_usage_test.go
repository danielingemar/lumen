package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// usageServer answers the account list the way Nextcloud does (paged, quota.used per account) and everything else as the
// shared fake does.
func usageServer(t *testing.T, n int, code int, disabledEvery int) (*httptest.Server, *int) {
	resetDeepChecks()
	f := &fakeNC{hits: map[string]int{}, mode: "cron", errsJSON: "[]", lastcron: fmt.Sprint(time.Now().Unix())}
	pages := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ocs/v2.php/cloud/users/details" {
			f.handler().ServeHTTP(w, r)
			return
		}
		if code != 0 {
			w.WriteHeader(code)
			return
		}
		pages++
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		users := map[string]any{}
		for i := off; i < off+limit && i < n; i++ {
			u := map[string]any{"enabled": !(disabledEvery > 0 && i%disabledEvery == 0), "quota": map[string]any{"used": 1000}}
			if i%7 == 0 { // Nextcloud sends a string for some accounts and leaves the quota out for ones that never logged in
				u["quota"] = map[string]any{"used": "500"}
			}
			if i%11 == 0 {
				u["quota"] = map[string]any{}
			}
			users[fmt.Sprintf("u%d", i)] = u
		}
		json.NewEncoder(w).Encode(map[string]any{"ocs": map[string]any{"meta": map[string]any{"statuscode": 200}, "data": map[string]any{"users": users}}})
	}))
	t.Cleanup(ts.Close)
	return ts, &pages
}

func TestStorageUsedIsAddedUpOverAllPages(t *testing.T) {
	ts, pages := usageServer(t, 1203, 0, 0)
	pts, _ := CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL, Username: "monitor", Password: "pw"}, time.Now().UnixNano())
	var want float64
	for i := 0; i < 1203; i++ {
		switch {
		case i%11 == 0:
		case i%7 == 0:
			want += 500
		default:
			want += 1000
		}
	}
	got := byName(pts)["nextcloud_storage_used_bytes"]
	if got.Value != want || got.Attrs["instance"] == "" {
		t.Fatalf("want %v: %+v", want, got)
	}
	if *pages != 3 {
		t.Fatalf("1203 accounts are three pages of 500, got %d requests", *pages)
	}
	var en, dis float64 = -1, -1
	for _, p := range pts {
		if p.Name == "nextcloud_accounts" {
			if p.Attrs["state"] == "enabled" {
				en = p.Value
			} else {
				dis = p.Value
			}
		}
	}
	if en != 1203 || dis != 0 {
		t.Fatalf("accounts %v/%v", en, dis)
	}
}

func TestDisabledAccountsAreCountedSeparately(t *testing.T) {
	ts, _ := usageServer(t, 10, 0, 5) // accounts 0 and 5
	pts, _ := CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL, Username: "monitor", Password: "pw"}, time.Now().UnixNano())
	for _, p := range pts {
		if p.Name == "nextcloud_accounts" && p.Attrs["state"] == "disabled" && p.Value != 2 {
			t.Fatalf("%+v", p)
		}
	}
}

func TestStorageUsedNeedsAnAdminAndIsCached(t *testing.T) {
	ts, _ := usageServer(t, 5, 403, 0)
	pts, err := CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL, Username: "monitor", Password: "pw"}, time.Now().UnixNano())
	if _, ok := byName(pts)["nextcloud_storage_used_bytes"]; ok {
		t.Fatal("a refused login must not report a number")
	}
	if err == nil {
		t.Fatal("the reason should be reported")
	}
	// without a login nothing is asked at all
	ts, pages := usageServer(t, 5, 0, 0)
	CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL}, time.Now().UnixNano())
	if *pages != 0 {
		t.Fatal("no login, no account list")
	}
	// and a second collection right after the first uses the cached answer
	CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL, Username: "monitor", Password: "pw"}, time.Now().UnixNano())
	CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL, Username: "monitor", Password: "pw"}, time.Now().UnixNano())
	if *pages != 1 {
		t.Fatalf("expected one request, got %d", *pages)
	}
}
