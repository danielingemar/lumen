// Package audit keeps a record of who did what: the operations of the people who run the installation (creating,
// suspending and removing tenants, changing limits, going into a tenant) and everything done while inside a tenant.
// A tenant's administrators can read the entries about their own tenant, so support access is never invisible.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
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
	// Summary is what happened in words ("created user alice"); Via is how the person was signed in; IP is where from.
	Summary string `json:"summary,omitempty"`
	Via     string `json:"via,omitempty"` // password | api key | oidc | support
	IP      string `json:"ip,omitempty"`
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
	e.Detail, e.Target, e.Summary, e.Actor, e.Via, e.IP = clip(e.Detail, 400), clip(e.Target, 200), clip(e.Summary, 300), clip(e.Actor, 120), clip(e.Via, 20), clip(e.IP, 64)
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

// Filter narrows a search of the log.
type Filter struct {
	Actor    string    // exactly this person (without regard to case)
	Q        string    // this text in the summary, action, target or detail (without regard to case)
	From, To time.Time // zero = no limit
	Limit    int       // 0 = 200; at most 5000
}

// Query returns the newest entries of a tenant that match, newest first.
func (l *Log) Query(tenant string, f Filter) []Entry {
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 5000 {
		limit = 5000
	}
	q := strings.ToLower(f.Q)
	var out []Entry
	for _, e := range l.List(tenant, 0) { // newest first
		if f.Actor != "" && !strings.EqualFold(e.Actor, f.Actor) {
			continue
		}
		if !f.From.IsZero() && e.Time.Before(f.From) || !f.To.IsZero() && e.Time.After(f.To) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(e.Summary+" "+e.Action+" "+e.Target+" "+e.Detail), q) {
			continue
		}
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// Retain keeps what is newer than maxAge (when it is above zero) and at most keep entries per tenant, and deletes the rest.
func (l *Log) Retain(keep int, maxAge time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	all := l.List("", 0)
	seen := map[string]int{}
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cutoff := time.Time{}
	if maxAge > 0 {
		cutoff = l.Now().Add(-maxAge)
	}
	for _, e := range all { // newest first
		seen[e.Tenant]++
		if (keep > 0 && seen[e.Tenant] > keep) || (!cutoff.IsZero() && e.Time.Before(cutoff)) {
			_ = l.DB.Delete(c, coll, e.ID)
		}
	}
}
