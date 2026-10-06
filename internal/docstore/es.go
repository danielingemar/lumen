package docstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type ESConfig struct {
	URL      string // e.g. http://elasticsearch:9200
	User     string
	Password string
	APIKey   string // alternative to user/password
	Prefix   string // index prefix, default "lumen"
}

// ES talks to Elasticsearch (or OpenSearch: only the basic _doc/_create/_search APIs are used).
// Writes use refresh=wait_for so a document is searchable when the call returns; reads by id are
// real-time. Optimistic concurrency uses _seq_no/_primary_term. Note: Elasticsearch has no
// multi-document transactions; every operation here touches one document.
type ES struct {
	cfg ESConfig
	c   *http.Client
}

func NewES(cfg ESConfig) *ES {
	if cfg.Prefix == "" {
		cfg.Prefix = "lumen"
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	return &ES{cfg: cfg, c: &http.Client{Timeout: 10 * time.Second}}
}

func (e *ES) Name() string { return "elasticsearch:" + e.cfg.URL }

// mappings: filter fields are keyword; large or secret-ish fields are stored but not indexed.
var mappings = map[string]string{
	"users":          `{"dynamic":false,"properties":{"name":{"type":"keyword"},"tenant":{"type":"keyword"},"hash":{"type":"keyword","index":false},"created":{"type":"date"}}}`,
	"keys":           `{"dynamic":false,"properties":{"id":{"type":"keyword"},"tenant":{"type":"keyword"},"name":{"type":"keyword"},"prefix":{"type":"keyword"},"hash":{"type":"keyword","index":false},"created":{"type":"date"}}}`,
	"dashboards":     `{"dynamic":false,"properties":{"id":{"type":"keyword"},"tenant":{"type":"keyword"},"name":{"type":"keyword"},"updated":{"type":"date"},"body":{"type":"object","enabled":false}}}`,
	"meta":           `{"dynamic":false,"properties":{"value":{"type":"keyword","index":false}}}`,
	"alert_rules":    `{"dynamic":false,"properties":{"id":{"type":"keyword"},"tenant":{"type":"keyword"}}}`,
	"alert_state":    `{"dynamic":false,"properties":{"tenant":{"type":"keyword"},"rule_id":{"type":"keyword"}}}`,
	"alert_events":   `{"dynamic":false,"properties":{"tenant":{"type":"keyword"},"time":{"type":"date"}}}`,
	"alert_silences": `{"dynamic":false,"properties":{"tenant":{"type":"keyword"}}}`,
	"alert_channels": `{"dynamic":false,"properties":{"id":{"type":"keyword"},"tenant":{"type":"keyword"}}}`,
	"groups":         `{"dynamic":false,"properties":{"id":{"type":"keyword"},"tenant":{"type":"keyword"},"name":{"type":"keyword"},"created":{"type":"date"},"perms":{"type":"object","enabled":false}}}`,
	"hosts":          `{"dynamic":false,"properties":{"tenant":{"type":"keyword"},"host":{"type":"keyword"},"updated":{"type":"date"}}}`,
	"instances":      `{"dynamic":false,"properties":{"id":{"type":"keyword"},"tenant":{"type":"keyword"},"name":{"type":"keyword"},"host":{"type":"keyword"},"updated":{"type":"date"}}}`,
	"instances_gone": `{"dynamic":false,"properties":{"tenant":{"type":"keyword"},"key":{"type":"keyword"},"at":{"type":"date"}}}`,
	"agent_updates":  `{"dynamic":false,"properties":{"tenant":{"type":"keyword"},"host":{"type":"keyword"},"at":{"type":"date"}}}`,
	"tenants":        `{"dynamic":false,"properties":{"id":{"type":"keyword"},"status":{"type":"keyword"}}}`,
	"usage":          `{"dynamic":false,"properties":{"tenant":{"type":"keyword"},"day":{"type":"keyword"}}}`,
	"audit":          `{"dynamic":false,"properties":{"tenant":{"type":"keyword"},"time":{"type":"date"}}}`,
	"support_grants": `{"dynamic":false,"properties":{"tenant":{"type":"keyword"}}}`,
}

// genericMapping is used for a collection that has no entry above (it should have one, and a test says so): only the
// tenant is searchable, which is all the code ever filters on. Without a mapping Elasticsearch would guess, and a guessed
// text field does not match an exact "term" filter.
const genericMapping = `{"dynamic":false,"properties":{"tenant":{"type":"keyword"}}}`

func mappingFor(coll string) string {
	if m, ok := mappings[coll]; ok {
		return m
	}
	return genericMapping
}

// createIndex creates one collection's index. It is safe to call when the index exists.
func (e *ES) createIndex(ctx context.Context, coll string) error {
	code, b, err := e.do(ctx, "PUT", "/"+e.index(coll), json.RawMessage(`{"settings":{"number_of_shards":1,"number_of_replicas":0},"mappings":`+mappingFor(coll)+`}`))
	if err != nil {
		return err
	}
	if code == 400 && bytes.Contains(b, []byte("resource_already_exists_exception")) {
		return nil
	}
	if code == 401 || code == 403 {
		return fmt.Errorf("elasticsearch rejected the credentials (HTTP %d)", code)
	}
	if code >= 300 {
		return fmt.Errorf("creating index %s: HTTP %d: %s", e.index(coll), code, snippet(b))
	}
	return nil
}

func noIndex(code int, b []byte) bool {
	return code == 404 && bytes.Contains(b, []byte("index_not_found_exception"))
}

func (e *ES) index(coll string) string { return e.cfg.Prefix + "-" + coll }

func (e *ES) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.cfg.URL+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch {
	case e.cfg.APIKey != "":
		req.Header.Set("Authorization", "ApiKey "+e.cfg.APIKey)
	case e.cfg.User != "":
		req.SetBasicAuth(e.cfg.User, e.cfg.Password)
	}
	resp, err := e.c.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("elasticsearch unreachable: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, b, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// EnsureIndices creates the indices with their mappings (idempotent). Call at startup.
func (e *ES) EnsureIndices(ctx context.Context) error {
	for coll := range mappings {
		if err := e.createIndex(ctx, coll); err != nil {
			return err
		}
	}
	return nil
}

func (e *ES) Ping(ctx context.Context) error {
	code, b, err := e.do(ctx, "GET", "/", nil)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("elasticsearch HTTP %d: %s", code, snippet(b))
	}
	return nil
}

type hit struct {
	ID     string          `json:"_id"`
	Seq    *int64          `json:"_seq_no"`
	Term   *int64          `json:"_primary_term"`
	Source json.RawMessage `json:"_source"`
}

func (h hit) doc() Doc {
	v := ""
	if h.Seq != nil && h.Term != nil {
		v = fmt.Sprintf("%d:%d", *h.Seq, *h.Term)
	}
	return Doc{ID: h.ID, Version: v, Data: h.Source}
}

func (e *ES) Get(ctx context.Context, coll, id string) (Doc, error) {
	code, b, err := e.do(ctx, "GET", "/"+e.index(coll)+"/_doc/"+url.PathEscape(id), nil)
	if err != nil {
		return Doc{}, err
	}
	if code == 404 {
		return Doc{}, ErrNotFound
	}
	if code != 200 {
		return Doc{}, fmt.Errorf("elasticsearch get: HTTP %d: %s", code, snippet(b))
	}
	var h hit
	if err := json.Unmarshal(b, &h); err != nil {
		return Doc{}, err
	}
	return h.doc(), nil
}

func (e *ES) write(ctx context.Context, op, coll, id string, v any, version string) error {
	q := "?refresh=wait_for"
	if version != "" {
		seq, term, ok := strings.Cut(version, ":")
		if _, err := strconv.ParseInt(seq, 10, 64); !ok || err != nil {
			return ErrConflict
		}
		q += "&if_seq_no=" + url.QueryEscape(seq) + "&if_primary_term=" + url.QueryEscape(term)
	}
	code, b, err := e.do(ctx, "PUT", "/"+e.index(coll)+"/"+op+"/"+url.PathEscape(id)+q, v)
	if err == nil && noIndex(code, b) && version == "" { // a collection that has no index yet (added by a newer version of Lumen)
		if cerr := e.createIndex(ctx, coll); cerr != nil {
			return cerr
		}
		code, b, err = e.do(ctx, "PUT", "/"+e.index(coll)+"/"+op+"/"+url.PathEscape(id)+q, v)
	}
	if err != nil {
		return err
	}
	switch {
	case code == 409 && op == "_create":
		return ErrExists
	case code == 409, code == 404 && version != "":
		return ErrConflict
	case code >= 300:
		return fmt.Errorf("elasticsearch write: HTTP %d: %s", code, snippet(b))
	}
	return nil
}

func (e *ES) Create(ctx context.Context, coll, id string, v any) error {
	return e.write(ctx, "_create", coll, id, v, "")
}
func (e *ES) Put(ctx context.Context, coll, id string, v any, version string) error {
	return e.write(ctx, "_doc", coll, id, v, version)
}

func (e *ES) Delete(ctx context.Context, coll, id string) error {
	code, b, err := e.do(ctx, "DELETE", "/"+e.index(coll)+"/_doc/"+url.PathEscape(id)+"?refresh=wait_for", nil)
	if err != nil {
		return err
	}
	if code >= 300 && code != 404 {
		return fmt.Errorf("elasticsearch delete: HTTP %d: %s", code, snippet(b))
	}
	return nil
}

func (e *ES) List(ctx context.Context, coll string, filter map[string]string, limit int) ([]Doc, error) {
	if limit <= 0 || limit > 10000 { // 10 000 is Elasticsearch's default result window
		limit = 10000
	}
	filters := []map[string]any{}
	for k, v := range filter {
		filters = append(filters, map[string]any{"term": map[string]any{k: v}})
	}
	code, b, err := e.do(ctx, "POST", "/"+e.index(coll)+"/_search", map[string]any{
		"size": limit, "seq_no_primary_term": true,
		"query": map[string]any{"bool": map[string]any{"filter": filters}},
	})
	if err != nil {
		return nil, err
	}
	if noIndex(code, b) {
		return nil, nil // nothing has ever been stored in this collection
	}
	if code != 200 {
		return nil, fmt.Errorf("elasticsearch search: HTTP %d: %s", code, snippet(b))
	}
	var r struct {
		Hits struct {
			Hits []hit `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	out := make([]Doc, 0, len(r.Hits.Hits))
	for _, h := range r.Hits.Hits {
		out = append(out, h.doc())
	}
	return out, nil
}
