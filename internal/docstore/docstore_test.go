package docstore

import (
	"context"
	"errors"
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
