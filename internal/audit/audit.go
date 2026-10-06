// Package audit keeps a record of who did what: the operations of the people who run the installation (creating,
// suspending and removing tenants, changing limits, going into a tenant) and everything done while inside a tenant.
// A tenant's administrators can read the entries about their own tenant, so support access is never invisible.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
)

const coll = "audit"

// Entry is one thing that happened.
type Entry struct {
	ID     string    `json:"id"`
	Time   time.Time `json:"time"`
	Tenant string    `json:"tenant"`           // the tenant the entry is about (it can read it)
	Actor  string    `json:"actor"`            // who did it
	Acting bool      `json:"acting,omitempty"` // an operator inside the tenant
	Action string    `json:"action"`           // for example tenant.create, support.enter, http.POST
	Target string    `json:"target,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// Log writes and reads entries.
type Log struct {
	DB  docstore.Backend
	Now func() time.Time
	mu  sync.Mutex
}

func New(db docstore.Backend) *Log { return &Log{DB: db, Now: time.Now} }

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Add records an entry. A failure to write is not allowed to stop what is being audited, but it is reported to the caller.
func (l *Log) Add(e Entry) error {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	e.ID, e.Time = hex.EncodeToString(b), l.Now().UTC()
	e.Detail, e.Target = clip(e.Detail, 400), clip(e.Target, 200)
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return l.DB.Put(c, coll, e.ID, e, "")
}

// List returns the newest entries of a tenant, or of everyone when tenant is empty.
func (l *Log) List(tenant string, limit int) []Entry {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var f map[string]string
	if tenant != "" {
		f = map[string]string{"tenant": tenant}
	}
	docs, _ := l.DB.List(c, coll, f, 10000)
	out := make([]Entry, 0, len(docs))
	for _, d := range docs {
		var e Entry
		if d.Decode(&e) == nil {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Prune keeps the newest keep entries per tenant.
func (l *Log) Prune(keep int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	all := l.List("", 0)
	seen := map[string]int{}
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, e := range all { // newest first
		seen[e.Tenant]++
		if seen[e.Tenant] > keep {
			_ = l.DB.Delete(c, coll, e.ID)
		}
	}
}
