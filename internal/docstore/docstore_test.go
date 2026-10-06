package docstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/danielingemar/lumen/internal/docstore/estest"
)

func backends(t *testing.T) map[string]Backend {
	ts, _ := estest.New()
	t.Cleanup(ts.Close)
	es := NewES(ESConfig{URL: ts.URL, User: "elastic", Password: "pw", Prefix: "t"})
	if err := es.EnsureIndices(context.Background()); err != nil {
		t.Fatal(err)
	}
	f, err := OpenFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return map[string]Backend{"file": f, "elasticsearch(fake)": es}
}

type rec struct {
	Name   string `json:"name"`
	Tenant string `json:"tenant"`
	N      int    `json:"n"`
}

func TestBackendContract(t *testing.T) {
	ctx := context.Background()
	for name, b := range backends(t) {
		t.Run(name, func(t *testing.T) {
			if _, err := b.Get(ctx, "users", "nobody"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing doc must be ErrNotFound, got %v", err)
			}
			if err := b.Create(ctx, "users", "a@x.com", rec{"a", "acme", 1}); err != nil { // id with special characters
				t.Fatal(err)
			}
			if err := b.Create(ctx, "users", "a@x.com", rec{"a", "acme", 2}); !errors.Is(err, ErrExists) {
				t.Fatalf("duplicate create must be ErrExists, got %v", err)
			}
			d, err := b.Get(ctx, "users", "a@x.com")
			var r rec
			if err != nil || d.Decode(&r) != nil || r.N != 1 || d.Version == "" {
				t.Fatalf("get: %+v %v %+v", d, err, r)
			}
			// optimistic concurrency: two writers read the same version, the second loses
			if err := b.Put(ctx, "users", "a@x.com", rec{"a", "acme", 10}, d.Version); err != nil {
				t.Fatal(err)
			}
			if err := b.Put(ctx, "users", "a@x.com", rec{"a", "acme", 20}, d.Version); !errors.Is(err, ErrConflict) {
				t.Fatalf("stale version must be ErrConflict, got %v", err)
			}
			if err := b.Put(ctx, "users", "ghost", rec{}, "5:1"); !errors.Is(err, ErrConflict) {
				t.Fatalf("conditional put on a missing doc must conflict, got %v", err)
			}
			if err := b.Put(ctx, "users", "b", rec{"b", "globex", 3}, ""); err != nil { // unconditional upsert
				t.Fatal(err)
			}
			list, _ := b.List(ctx, "users", map[string]string{"tenant": "acme"}, 10)
			if len(list) != 1 || list[0].ID != "a@x.com" {
				t.Fatalf("filtered list: %+v", list)
			}
			if all, _ := b.List(ctx, "users", nil, 10); len(all) != 2 {
				t.Fatalf("unfiltered list: %+v", all)
			}
			if err := b.Delete(ctx, "users", "a@x.com"); err != nil {
				t.Fatal(err)
			}
			if err := b.Delete(ctx, "users", "a@x.com"); err != nil {
				t.Fatalf("deleting twice must not fail: %v", err)
			}
			if _, err := b.Get(ctx, "users", "a@x.com"); !errors.Is(err, ErrNotFound) {
				t.Fatal("deleted doc must be gone")
			}
			if b.Ping(ctx) != nil {
				t.Fatal("ping failed")
			}
		})
	}
}

func TestESAuthAndErrors(t *testing.T) {
	ts, f := estest.New()
	defer ts.Close()
	es := NewES(ESConfig{URL: ts.URL, User: "elastic", Password: "pw"})
	es.EnsureIndices(context.Background())
	if f.AuthSeen != "basic:elastic:pw" {
		t.Fatalf("basic auth not sent: %q", f.AuthSeen)
	}
	es2 := NewES(ESConfig{URL: ts.URL, APIKey: "abc123"})
	es2.Ping(context.Background())
	if f.AuthSeen != "ApiKey abc123" {
		t.Fatalf("api key auth not sent: %q", f.AuthSeen)
	}
	if err := es.EnsureIndices(context.Background()); err != nil { // second run: indices exist
		t.Fatalf("EnsureIndices must be idempotent: %v", err)
	}
	down := NewES(ESConfig{URL: "http://127.0.0.1:1"})
	if err := down.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("unreachable ES must give a clear error, got %v", err)
	}
}

// A collection that has no index yet (one a newer version of Lumen added) must get one on its first write, also when
// Elasticsearch is set not to create indices by itself. This was missed once: "the settings store is unavailable (the reason is in the server log: docker compose logs lumen, look for \"registry error\")".
func TestACollectionWithoutAnIndexGetsOneOnFirstWrite(t *testing.T) {
	ts, fake := estest.New()
	defer ts.Close()
	es := NewES(ESConfig{URL: ts.URL, User: "elastic", Password: "pw", Prefix: "t"}) // EnsureIndices has NOT been run
	ctx := context.Background()
	if docs, err := es.List(ctx, "agent_updates", map[string]string{"tenant": "acme"}, 10); err != nil || len(docs) != 0 {
		t.Fatalf("a collection nobody has written to is empty, not an error: %v %v", docs, err)
	}
	if err := es.Put(ctx, "agent_updates", "acme:web1", map[string]any{"tenant": "acme", "host": "web1"}, ""); err != nil {
		t.Fatalf("the first write creates the index: %v", err)
	}
	if m := fake.Mappings["t-agent_updates"]; !strings.Contains(m, `"host":{"type":"keyword"}`) || !strings.Contains(m, `"tenant":{"type":"keyword"}`) {
		t.Fatalf("with its own mapping, so that exact filters work: %s", m)
	}
	if docs, err := es.List(ctx, "agent_updates", map[string]string{"tenant": "acme"}, 10); err != nil || len(docs) != 1 {
		t.Fatalf("%v %v", docs, err)
	}
	// one that has no mapping of its own still works, with the generic one
	if err := es.Create(ctx, "zz_future", "x", map[string]any{"tenant": "acme"}); err != nil {
		t.Fatal(err)
	}
	if m := fake.Mappings["t-zz_future"]; !strings.Contains(m, `"tenant":{"type":"keyword"}`) {
		t.Fatalf("%s", m)
	}
	// a write that depends on a version of a document that does not exist is still a conflict, not a created index
	if err := es.Put(ctx, "zz_other", "x", map[string]any{}, "5:1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("%v", err)
	}
	if _, ok := fake.Mappings["t-zz_other"]; ok {
		t.Fatal("no index is created for a conditional write")
	}
}

// Every collection the code uses must have a mapping in the Elasticsearch store, or filtering on it would silently
// not work. The collections are found by reading the source, so that a new one cannot be forgotten.
func TestEveryCollectionInTheCodeHasAMapping(t *testing.T) {
	re := regexp.MustCompile(`(?m)\b(?:coll[A-Za-z]*|[a-z]*Coll)\s*=\s*"([a-z_]+)"`)
	found := map[string]string{}
	err := filepath.Walk("..", func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		b, _ := os.ReadFile(p)
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			found[m[1]] = p
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) < 12 {
		t.Fatalf("the scan found only %d collections: the pattern no longer matches how they are declared: %v", len(found), found)
	}
	for name, file := range found {
		if _, ok := mappings[name]; !ok {
			t.Errorf("the collection %q (declared in %s) has no mapping in internal/docstore/es.go", name, file)
		}
	}
}

func TestTheNewCollectionsRoundTripInEveryBackend(t *testing.T) {
	for name, b := range backends(t) {
		for _, coll := range []string{"tenants", "usage", "audit", "support_grants", "instances_gone", "agent_updates"} {
			ctx := context.Background()
			if err := b.Put(ctx, coll, "a", rec{Name: "a", Tenant: "acme"}, ""); err != nil {
				t.Fatalf("%s %s: %v", name, coll, err)
			}
			if err := b.Put(ctx, coll, "b", rec{Name: "b", Tenant: "globex"}, ""); err != nil {
				t.Fatalf("%s %s: %v", name, coll, err)
			}
			docs, err := b.List(ctx, coll, map[string]string{"tenant": "acme"}, 10)
			if err != nil || len(docs) != 1 || docs[0].ID != "a" {
				t.Fatalf("%s %s: filtering by tenant: %v %v", name, coll, docs, err)
			}
			if err := b.Delete(ctx, coll, "a"); err != nil {
				t.Fatalf("%s %s: %v", name, coll, err)
			}
		}
	}
}
