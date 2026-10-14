package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Nextcloud's serverinfo only tells about the apps and a newer Nextcloud when asked with skipApps=false&skipUpdate=false.
func TestAppsAndUpdateAreAskedForAndNotEveryTime(t *testing.T) {
	var mu sync.Mutex
	var asked, full int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status.php":
			w.Write([]byte(`{"installed":true,"maintenance":false,"versionstring":"35.0.1"}`))
		case "/ocs/v2.php/apps/serverinfo/api/v1/info":
			mu.Lock()
			asked++
			wantsAll := r.URL.Query().Get("skipApps") == "false" && r.URL.Query().Get("skipUpdate") == "false"
			if wantsAll {
				full++
			}
			mu.Unlock()
			system := `"nextcloud":{"system":{"version":"35.0.1.1","freespace":1000}`
			if wantsAll {
				system = `"nextcloud":{"system":{"version":"35.0.1.1","freespace":1000,"apps":{"num_installed":57,"num_updates_available":2},"update":{"available":true,"available_version":"35.0.2"}}`
			}
			w.Write([]byte(`{"ocs":{"data":{` + system + `,"storage":{"num_users":2,"num_files":211}}}}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer ts.Close()
	target := NextcloudTarget{URL: ts.URL, Service: "nc", Token: "x"}

	pts, err := CollectNextcloud(context.Background(), target, 1)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]float64{"nextcloud_apps_installed": 57, "nextcloud_apps_updates_available": 2, "nextcloud_update_available": 1, "nextcloud_users": 2} {
		if p, ok := find(pts, name); !ok || p.Value != want {
			t.Errorf("%s: %v %v", name, p, ok)
		}
	}
	// the next collections, a few seconds later, ask the ordinary question: the instance is not worked every time
	for i := 0; i < 3; i++ {
		pts, err = CollectNextcloud(context.Background(), target, 2)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := find(pts, "nextcloud_users"); !ok {
			t.Fatal("the ordinary numbers are always reported")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if full != 1 || asked != 4 {
		t.Fatalf("asked %d times, the full answer %d times", asked, full)
	}
}

// An instance that refuses or fails the full question still gets its ordinary numbers in the same collection.
func TestFullQuestionFailingFallsBackToTheOrdinaryOne(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status.php":
			w.Write([]byte(`{"installed":true,"versionstring":"35.0.1"}`))
		case "/ocs/v2.php/apps/serverinfo/api/v1/info":
			if r.URL.Query().Get("skipApps") == "false" {
				w.WriteHeader(500)
				return
			}
			w.Write([]byte(`{"ocs":{"data":{"nextcloud":{"storage":{"num_users":5}}}}}`))
		}
	}))
	defer ts.Close()
	pts, err := CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL, Service: "nc", Token: "x"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := find(pts, "nextcloud_users"); !ok || p.Value != 5 {
		t.Fatalf("%v %v", p, ok)
	}
}
