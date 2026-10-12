package agent

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestParseDockerNetworkAddsTheInterfaces(t *testing.T) {
	rx, tx, ok := ParseDockerNetwork([]byte(`{"networks":{"eth0":{"rx_bytes":100,"tx_bytes":2000},"eth1":{"rx_bytes":50,"tx_bytes":500}}}`))
	if !ok || rx != 150 || tx != 2500 {
		t.Fatal(rx, tx, ok)
	}
	for _, none := range []string{`{}`, `{"networks":{}}`, `not json`, `{"networks":null}`} {
		if _, _, ok := ParseDockerNetwork([]byte(none)); ok {
			t.Errorf("%q has no counters of its own (host network, or no network)", none)
		}
	}
}

func fakeDocker(t *testing.T) string {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("no unix sockets here:", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"Id":"aaa","Names":["/nextcloud-ks"],"Image":"nextcloud","State":"running"},
			{"Id":"bbb","Names":["/db-ks"],"Image":"postgres","State":"running"},
			{"Id":"ccc","Names":["/stopped"],"Image":"x","State":"exited"},
			{"Id":"ddd","Names":["/hostnet"],"Image":"x","State":"running"}]`))
	})
	mux.HandleFunc("/containers/aaa/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stream") != "false" || r.URL.Query().Get("one-shot") != "true" {
			w.WriteHeader(400) // a streaming call would hang the agent
			return
		}
		w.Write([]byte(`{"networks":{"eth0":{"rx_bytes":1000,"tx_bytes":9000}}}`))
	})
	mux.HandleFunc("/containers/bbb/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"networks":{"eth0":{"rx_bytes":7,"tx_bytes":8}}}`))
	})
	mux.HandleFunc("/containers/ddd/stats", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"read":"x"}`)) })
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close(); os.Remove(sock) })
	return sock
}

func TestContainerNetworkCountersPerRunningContainer(t *testing.T) {
	pts, err := CollectContainerNetwork(context.Background(), fakeDocker(t), 5)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, p := range pts {
		if p.Name != "container_network_bytes" || p.Type != "sum" || p.Service != "host" {
			t.Fatalf("%+v", p)
		}
		got[p.Attrs["container"]+"/"+p.Attrs["direction"]] = p.Value
	}
	want := map[string]float64{"nextcloud-ks/transmit": 9000, "nextcloud-ks/receive": 1000, "db-ks/transmit": 8, "db-ks/receive": 7}
	if len(got) != len(want) {
		t.Fatalf("a stopped container and one on the host network have no counters: %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestContainerNetworkWithoutDocker(t *testing.T) {
	if _, err := CollectContainerNetwork(context.Background(), "/nonexistent/docker.sock", 1); err != ErrNoDocker {
		t.Fatal(err)
	}
}
