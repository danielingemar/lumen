// Package docstore is a minimal document-store abstraction for small, frequently changing records
// (users, API keys, dashboards, settings). Backends: Elasticsearch/OpenSearch, or a JSON file when
// no Elasticsearch is configured. Telemetry (traces, logs, metrics) does NOT go through here.
package docstore

import (
	"context"
	"encoding/json"
	"errors"
)

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
	ErrConflict = errors.New("version conflict: the document was changed by someone else")
)

// Doc is a stored document. Version is opaque: pass it back to Put for optimistic concurrency.
type Doc struct {
	ID      string
	Version string
	Data    json.RawMessage
}

func (d Doc) Decode(v any) error { return json.Unmarshal(d.Data, v) }

type Backend interface {
	Get(ctx context.Context, coll, id string) (Doc, error)
	// Create fails with ErrExists if the id is taken.
	Create(ctx context.Context, coll, id string, v any) error
	// Put replaces a document. version == "" means unconditional upsert; otherwise the write only
	// succeeds if the stored version matches, else ErrConflict.
	Put(ctx context.Context, coll, id string, v any, version string) error
	Delete(ctx context.Context, coll, id string) error
	// List returns documents whose top-level string fields equal every filter entry.
	List(ctx context.Context, coll string, filter map[string]string, limit int) ([]Doc, error)
	Ping(ctx context.Context) error
	Name() string
}
