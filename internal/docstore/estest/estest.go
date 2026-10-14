// Package estest provides a tiny in-memory fake of the Elasticsearch REST API for tests. It implements only what
// Lumen's client uses (index create, _doc/_create/_search, optimistic concurrency) and is NOT a substitute for
// testing against a real Elasticsearch.
package estest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

type fakeDoc struct {
	src  json.RawMessage
	seq  int64
	term int64
}

// Fake implements just enough of the Elasticsearch REST API to check the client's requests and
// concurrency handling. It is NOT a substitute for testing against a real Elasticsearch.
type Fake struct {
	mu       sync.Mutex
	idx      map[string]map[string]*fakeDoc
	seq      int64
	AuthSeen string
	Mappings map[string]string // the body each index was created with, by index name
	// what the health endpoints answer; the zero values are a healthy single node
	ClusterStatus string
	Unassigned    int
	DiskPercent   string // "disk.percent" of the one node, as Elasticsearch writes it (a string)
	DiskAvail     string
	DiskTotal     string
	Nodes         int                 // number_of_nodes; 0 means one
	ShardRows     []map[string]string // what _cat/shards answers (index, prirep, state)
	SettingsPuts  []string            // the index patterns that had their settings changed, and to what
}

// New starts a fake Elasticsearch. Close the returned server when done.
func New() (*httptest.Server, *Fake) {
	f := &Fake{idx: map[string]map[string]*fakeDoc{}, Mappings: map[string]string{}}
	return httptest.NewServer(f), f
}

func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, p, ok := r.BasicAuth(); ok {
		f.AuthSeen = "basic:" + u + ":" + p
	} else {
		f.AuthSeen = r.Header.Get("Authorization")
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	reply := func(code int, body string) { w.WriteHeader(code); w.Write([]byte(body)) }
	if r.URL.Path == "/" {
		reply(200, `{"version":{"number":"8.15.0"}}`)
		return
	}
	if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/_settings") {
		body, _ := io.ReadAll(r.Body)
		f.SettingsPuts = append(f.SettingsPuts, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/_settings")+" "+string(body))
		reply(200, `{"acknowledged":true}`)
		return
	}
	switch r.URL.Path {
	case "/_cluster/health":
		st, n := f.ClusterStatus, f.Nodes
		if st == "" {
			st = "green"
		}
		if n == 0 {
			n = 1
		}
		reply(200, fmt.Sprintf(`{"cluster_name":"docker-cluster","status":%q,"unassigned_shards":%d,"number_of_nodes":%d}`, st, f.Unassigned, n))
		return
	case "/_cat/shards":
		b, _ := json.Marshal(f.ShardRows)
		reply(200, string(b))
		return
	case "/_cat/allocation":
		dp, da, dt := f.DiskPercent, f.DiskAvail, f.DiskTotal
		if dp == "" {
			dp = "40"
		}
		if da == "" {
			da = "42949672960"
		}
		if dt == "" {
			dt = "107374182400"
		}
		reply(200, fmt.Sprintf(`[{"shards":"6","disk.indices":"1gb","disk.used":"40gb","disk.avail":%q,"disk.total":%q,"disk.percent":%q,"host":"172.18.0.2","ip":"172.18.0.2","node":"es-node-1"},{"shards":"1","node":"UNASSIGNED"}]`, da, dt, dp))
		return
	}
	idx := parts[0]
	if len(parts) == 1 && r.Method == "PUT" {
		if f.idx[idx] != nil {
			reply(400, `{"error":{"type":"resource_already_exists_exception"}}`)
			return
		}
		f.idx[idx] = map[string]*fakeDoc{}
		if body, err := io.ReadAll(r.Body); err == nil {
			f.Mappings[idx] = string(body)
		}
		reply(200, `{"acknowledged":true}`)
		return
	}
	if f.idx[idx] == nil {
		reply(404, `{"error":{"type":"index_not_found_exception"}}`)
		return
	}
	if len(parts) == 2 && parts[1] == "_search" {
		var q struct {
			Size  int `json:"size"`
			Query struct {
				Bool struct {
					Filter []map[string]map[string]string `json:"filter"`
				} `json:"bool"`
			} `json:"query"`
		}
		json.NewDecoder(r.Body).Decode(&q)
		hits := []map[string]any{}
	outer:
		for id, d := range f.idx[idx] {
			var m map[string]any
			json.Unmarshal(d.src, &m)
			for _, fl := range q.Query.Bool.Filter {
				for k, v := range fl["term"] {
					if s, _ := m[k].(string); s != v {
						continue outer
					}
				}
			}
			hits = append(hits, map[string]any{"_id": id, "_seq_no": d.seq, "_primary_term": d.term, "_source": json.RawMessage(d.src)})
		}
		b, _ := json.Marshal(map[string]any{"hits": map[string]any{"hits": hits}})
		reply(200, string(b))
		return
	}
	if len(parts) != 3 {
		reply(400, `{}`)
		return
	}
	op, id := parts[1], parts[2]
	cur := f.idx[idx][id]
	switch {
	case r.Method == "GET" && op == "_doc":
		if cur == nil {
			reply(404, `{"found":false}`)
			return
		}
		b, _ := json.Marshal(map[string]any{"_id": id, "found": true, "_seq_no": cur.seq, "_primary_term": cur.term, "_source": json.RawMessage(cur.src)})
		reply(200, string(b))
	case r.Method == "DELETE":
		if cur == nil {
			reply(404, `{"result":"not_found"}`)
			return
		}
		delete(f.idx[idx], id)
		reply(200, `{"result":"deleted"}`)
	case r.Method == "PUT":
		if r.URL.Query().Get("refresh") != "wait_for" {
			reply(400, `{"error":"writes must use refresh=wait_for"}`)
			return
		}
		if op == "_create" && cur != nil {
			reply(409, `{"error":{"type":"version_conflict_engine_exception"}}`)
			return
		}
		if s := r.URL.Query().Get("if_seq_no"); s != "" {
			seq, _ := strconv.ParseInt(s, 10, 64)
			term, _ := strconv.ParseInt(r.URL.Query().Get("if_primary_term"), 10, 64)
			if cur == nil || cur.seq != seq || cur.term != term {
				reply(409, `{"error":{"type":"version_conflict_engine_exception"}}`)
				return
			}
		}
		var raw json.RawMessage
		json.NewDecoder(r.Body).Decode(&raw)
		f.seq++
		f.idx[idx][id] = &fakeDoc{src: raw, seq: f.seq, term: 1}
		reply(201, `{"result":"created"}`)
	default:
		reply(400, `{}`)
	}
}
