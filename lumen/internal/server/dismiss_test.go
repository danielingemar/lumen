package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestARowCanBeTakenOffTheHomeList(t *testing.T) {
	ts, _, fs := regServer(t)
	ad, rd, ot := client(), client(), client()
	login(t, ad, ts, "admin", "admins-long-password")
	login(t, rd, ts, "reader", "readers-long-password")
	login(t, ot, ts, "other", "others-long-password")
	old := time.Now().UTC().Add(-3 * time.Hour).Format("2006-01-02 15:04:05.000000000")
	fs.servicesRows = []json.RawMessage{json.RawMessage(`{"svc":"old-box","last_seen":"` + old + `","signals":["metrics"]}`), json.RawMessage(`{"svc":"checkout","last_seen":"` + time.Now().UTC().Format("2006-01-02 15:04:05.000000000") + `","signals":["traces"]}`)}
	list := func(cl *http.Client) string {
		_, b := do(cl, "GET", ts.URL+"/api/v1/services", "")
		var out struct{ Data []struct{ Svc string } }
		json.Unmarshal(b, &out)
		var n []string
		for _, x := range out.Data {
			n = append(n, x.Svc)
		}
		return strings.Join(n, ",")
	}
	if got := list(ad); got != "old-box,checkout" {
		t.Fatal(got)
	}
	if c := code(rd, "POST", ts.URL+"/api/v1/services/dismiss", `{"name":"old-box"}`); c != 403 {
		t.Fatalf("a read-only user cannot take a row away: %d", c)
	}
	if c := code(ad, "POST", ts.URL+"/api/v1/services/dismiss", `{"name":"old-box"}`); c != 200 {
		t.Fatal(c)
	}
	if got := list(ad); got != "checkout" {
		t.Fatalf("the row is gone from the list: %q", got)
	}
	if got := list(ot); got != "old-box,checkout" {
		t.Fatalf("another tenant keeps its own list: %q", got)
	}
	// it comes back by itself if it reports again
	fs.servicesRows[0] = json.RawMessage(`{"svc":"old-box","last_seen":"` + time.Now().UTC().Add(time.Minute).Format("2006-01-02 15:04:05.000000000") + `","signals":["metrics"]}`)
	if got := list(ad); got != "old-box,checkout" {
		t.Fatalf("a row that reports again after it was taken away is back: %q", got)
	}
	for name, body := range map[string]string{"empty": `{"name":""}`, "long": `{"name":"` + strings.Repeat("a", 201) + `"}`, "control": `{"name":"a\u0001b"}`} {
		if c := code(ad, "POST", ts.URL+"/api/v1/services/dismiss", body); c != 400 {
			t.Errorf("%s: %d", name, c)
		}
	}
}
