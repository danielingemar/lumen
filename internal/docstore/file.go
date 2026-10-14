package docstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type fileEntry struct {
	V    int             `json:"v"`
	Data json.RawMessage `json:"data"`
}

// File keeps all collections in one JSON file (0600, atomic writes). It re-reads the file when it
// changes on disk, so the CLI and the server can share it. Suitable for a single small instance.
type File struct {
	mu    sync.Mutex
	path  string
	colls map[string]map[string]fileEntry
	mtime time.Time
	size  int64
}

func OpenFile(dir string) (*File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f := &File{path: filepath.Join(dir, "documents.json"), colls: map[string]map[string]fileEntry{}}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f, f.load()
}

func (f *File) Name() string { return "file:" + f.path }

func (f *File) load() error {
	st, err := os.Stat(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	b, err := os.ReadFile(f.path)
	if err != nil {
		return err
	}
	c := map[string]map[string]fileEntry{}
	if err := json.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("%s is corrupt: %w", f.path, err)
	}
	f.colls, f.mtime, f.size = c, st.ModTime(), st.Size()
	return nil
}

func (f *File) refresh() {
	if st, err := os.Stat(f.path); err == nil && (!st.ModTime().Equal(f.mtime) || st.Size() != f.size) {
		_ = f.load()
	}
}

func (f *File) save() error {
	b, err := json.MarshalIndent(f.colls, "", " ")
	if err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, f.path); err != nil {
		return err
	}
	if st, err := os.Stat(f.path); err == nil {
		f.mtime, f.size = st.ModTime(), st.Size()
	}
	return nil
}

func (f *File) Ping(context.Context) error { return nil }

func (f *File) Get(_ context.Context, coll, id string) (Doc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refresh()
	e, ok := f.colls[coll][id]
	if !ok {
		return Doc{}, ErrNotFound
	}
	return Doc{ID: id, Version: strconv.Itoa(e.V), Data: e.Data}, nil
}

func (f *File) write(coll, id string, v any, version string, create bool) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refresh()
	old, exists := f.colls[coll][id]
	switch {
	case create && exists:
		return ErrExists
	case version != "" && (!exists || strconv.Itoa(old.V) != version):
		return ErrConflict
	}
	if f.colls[coll] == nil {
		f.colls[coll] = map[string]fileEntry{}
	}
	f.colls[coll][id] = fileEntry{V: old.V + 1, Data: data}
	return f.save()
}

func (f *File) Create(_ context.Context, coll, id string, v any) error {
	return f.write(coll, id, v, "", true)
}
func (f *File) Put(_ context.Context, coll, id string, v any, version string) error {
	return f.write(coll, id, v, version, false)
}

func (f *File) Delete(_ context.Context, coll, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refresh()
	if _, ok := f.colls[coll][id]; !ok {
		return nil
	}
	delete(f.colls[coll], id)
	return f.save()
}

func (f *File) List(_ context.Context, coll string, filter map[string]string, limit int) ([]Doc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refresh()
	var out []Doc
outer:
	for id, e := range f.colls[coll] {
		if len(filter) > 0 {
			var m map[string]any
			if json.Unmarshal(e.Data, &m) != nil {
				continue
			}
			for k, want := range filter {
				if s, _ := m[k].(string); s != want {
					continue outer
				}
			}
		}
		out = append(out, Doc{ID: id, Version: strconv.Itoa(e.V), Data: e.Data})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}
