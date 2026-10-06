// Package tenants holds the records of the tenants of an installation: their status, limits, support-access setting and
// what is needed to offboard one. A tenant always existed implicitly (a name on users, keys and data); this package gives
// each one a record that the operator of the installation can manage.
package tenants

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/perm"
)

const (
	coll       = "tenants"
	grantsColl = "support_grants"

	Active      = "active"
	Suspended   = "suspended"
	Offboarding = "offboarding"
	Deleted     = "deleted" // a tombstone: the data is gone, the name stays taken

	SupportOff   = "off"   // an operator cannot go into the tenant
	SupportAsk   = "ask"   // only while a tenant administrator has granted access
	SupportAllow = "allow" // any time (always recorded)
)

var (
	ErrInvalid  = errors.New("invalid")
	ErrNotFound = errors.New("not found")
)

func invalid(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(f, a...)) }

// Quotas are limits per tenant. Zero means no limit. With Hard false they only warn; with Hard true they refuse.
type Quotas struct {
	Hosts             int   `json:"hosts"`
	Instances         int   `json:"instances"`
	Users             int   `json:"users"`
	Groups            int   `json:"groups"`
	Keys              int   `json:"keys"`
	Dashboards        int   `json:"dashboards"`
	IngestPerSec      int   `json:"ingest_per_sec"`       // items (spans, log lines, metric points) per second
	IngestBytesPerDay int64 `json:"ingest_bytes_per_day"` // accepted OTLP payload bytes per day (UTC)
	Hard              bool  `json:"hard"`
}

// Step is one part of an offboarding.
type Step struct {
	Name   string `json:"name"`
	Done   bool   `json:"done"`
	Detail string `json:"detail,omitempty"`
}

// Offboard is the progress of removing a tenant's data.
type Offboard struct {
	State    string    `json:"state"` // purging | done | failed
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
	Steps    []Step    `json:"steps"`
	Error    string    `json:"error,omitempty"`
}

// Tenant is one customer (or team) of the installation.
type Tenant struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Status          string    `json:"status"`
	Created         time.Time `json:"created"`
	Updated         time.Time `json:"updated"`
	Contact         string    `json:"contact,omitempty"`
	Notes           string    `json:"notes,omitempty"`
	Plan            string    `json:"plan,omitempty"`
	Quotas          Quotas    `json:"quotas"`
	SupportAccess   string    `json:"support_access"`
	SuspendIngest   string    `json:"suspend_ingest"` // what happens to incoming data while suspended: reject | drop
	SuspendedReason string    `json:"suspended_reason,omitempty"`
	SuspendedAt     time.Time `json:"suspended_at,omitempty"`
	Parent          string    `json:"parent,omitempty"` // reserved for tenants inside tenants; not used yet
	Reserved        bool      `json:"reserved,omitempty"`
	Migrated        bool      `json:"migrated,omitempty"` // made from an existing name, not created through the console
	Deleted         time.Time `json:"deleted,omitempty"`
	Offboard        *Offboard `json:"offboard,omitempty"`
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$`)

// ValidID checks the name of a new tenant: lower-case letters, digits and hyphens, 2-40 characters. It is used in
// addresses and file names, so it is strict. (Tenants that already existed keep whatever name they had.)
func ValidID(id string) error {
	if id == perm.OperatorTenant {
		return invalid("%q is reserved for the people who run the installation", id)
	}
	if !idRe.MatchString(id) {
		return invalid("the id must be 2-40 characters: lower-case letters, digits and hyphens, not starting or ending with a hyphen")
	}
	return nil
}

func (q Quotas) Validate() error {
	for name, n := range map[string]int{"hosts": q.Hosts, "instances": q.Instances, "users": q.Users, "groups": q.Groups, "keys": q.Keys, "dashboards": q.Dashboards, "ingest per second": q.IngestPerSec} {
		if n < 0 || n > 10_000_000 {
			return invalid("the limit for %s must be between 0 (no limit) and 10 000 000", name)
		}
	}
	if q.IngestBytesPerDay < 0 || q.IngestBytesPerDay > 1<<50 {
		return invalid("the daily volume limit is out of range")
	}
	return nil
}

func oneLine(s string, max int) bool { return len(s) <= max && !strings.ContainsAny(s, "\r\n\x00") }

// Service reads and writes tenant records. Lookups are cached for a few seconds because every request asks.
type Service struct {
	DB  docstore.Backend
	Now func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	t  Tenant
	ok bool
	at time.Time
}

func New(db docstore.Backend) *Service {
	return &Service{DB: db, Now: time.Now, cache: map[string]cached{}}
}

func ctx5() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 8*time.Second)
}

func (s *Service) invalidate(id string) { s.mu.Lock(); delete(s.cache, id); s.mu.Unlock() }

func (s *Service) load(id string) (Tenant, bool) {
	c, cancel := ctx5()
	defer cancel()
	d, err := s.DB.Get(c, coll, id)
	if err != nil {
		return Tenant{}, false
	}
	var t Tenant
	if d.Decode(&t) != nil {
		return Tenant{}, false
	}
	return t, true
}

// Get returns a tenant's record. The answer may be up to 5 seconds old.
func (s *Service) Get(id string) (Tenant, bool) {
	s.mu.Lock()
	if c, ok := s.cache[id]; ok && s.Now().Sub(c.at) < 5*time.Second {
		s.mu.Unlock()
		return c.t, c.ok
	}
	s.mu.Unlock()
	t, ok := s.load(id)
	s.mu.Lock()
	s.cache[id] = cached{t, ok, s.Now()}
	s.mu.Unlock()
	return t, ok
}

func (s *Service) put(t Tenant) error {
	c, cancel := ctx5()
	defer cancel()
	t.Updated = s.Now().UTC()
	err := s.DB.Put(c, coll, t.ID, t, "")
	s.invalidate(t.ID)
	return err
}

// List returns every tenant, the operator tenant first and then by name.
func (s *Service) List() []Tenant {
	c, cancel := ctx5()
	defer cancel()
	docs, _ := s.DB.List(c, coll, nil, 10000)
	out := make([]Tenant, 0, len(docs))
	for _, d := range docs {
		var t Tenant
		if d.Decode(&t) == nil {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Reserved != out[j].Reserved {
			return out[i].Reserved
		}
		return strings.ToLower(out[i].ID) < strings.ToLower(out[j].ID)
	})
	return out
}

// CreateIn is what a new tenant needs.
type CreateIn struct {
	ID, Name, Contact, Notes, Plan string
	Quotas                         Quotas
	SupportAccess                  string
}

// Create makes a tenant record. The first administrator is made by the caller.
func (s *Service) Create(in CreateIn) (Tenant, error) {
	if err := ValidID(in.ID); err != nil {
		return Tenant{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || !oneLine(in.Name, 80) || !oneLine(in.Contact, 200) || !oneLine(in.Plan, 40) || len(in.Notes) > 2000 {
		return Tenant{}, invalid("the name must be 1-80 characters on one line (contact 200, plan 40, notes 2000)")
	}
	if err := in.Quotas.Validate(); err != nil {
		return Tenant{}, err
	}
	switch in.SupportAccess {
	case "":
		in.SupportAccess = SupportAsk
	case SupportOff, SupportAsk, SupportAllow:
	default:
		return Tenant{}, invalid("support access must be off, ask or allow")
	}
	if old, ok := s.load(in.ID); ok {
		if old.Status == Deleted {
			return Tenant{}, invalid("the name %q belonged to a tenant that was removed on %s and cannot be used again", in.ID, old.Deleted.Format("2006-01-02"))
		}
		return Tenant{}, invalid("a tenant called %q already exists", in.ID)
	}
	now := s.Now().UTC()
	t := Tenant{ID: in.ID, Name: in.Name, Status: Active, Created: now, Contact: in.Contact, Notes: in.Notes, Plan: in.Plan, Quotas: in.Quotas, SupportAccess: in.SupportAccess, SuspendIngest: "reject"}
	c, cancel := ctx5()
	defer cancel()
	if err := s.DB.Create(c, coll, t.ID, t); err != nil {
		if errors.Is(err, docstore.ErrExists) {
			return Tenant{}, invalid("a tenant called %q already exists", in.ID)
		}
		return Tenant{}, err
	}
	s.invalidate(t.ID)
	return t, nil
}

// UpdateIn changes the descriptive parts of a tenant.
type UpdateIn struct {
	Name, Contact, Notes, Plan, SuspendIngest string
	Quotas                                    Quotas
}

func (s *Service) Update(id string, in UpdateIn) (Tenant, error) {
	t, ok := s.load(id)
	if !ok || t.Status == Deleted {
		return Tenant{}, ErrNotFound
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || !oneLine(in.Name, 80) || !oneLine(in.Contact, 200) || !oneLine(in.Plan, 40) || len(in.Notes) > 2000 {
		return Tenant{}, invalid("the name must be 1-80 characters on one line (contact 200, plan 40, notes 2000)")
	}
	if err := in.Quotas.Validate(); err != nil {
		return Tenant{}, err
	}
	switch in.SuspendIngest {
	case "":
		in.SuspendIngest = t.SuspendIngest
	case "reject", "drop":
	default:
		return Tenant{}, invalid("while suspended, incoming data is either rejected or dropped")
	}
	t.Name, t.Contact, t.Notes, t.Plan, t.Quotas, t.SuspendIngest = in.Name, in.Contact, in.Notes, in.Plan, in.Quotas, in.SuspendIngest
	return t, s.put(t)
}

// SetStatus suspends or resumes a tenant. The operator tenant cannot be suspended: that would lock everyone out.
func (s *Service) SetStatus(id, status, reason string) (Tenant, error) {
	t, ok := s.load(id)
	if !ok || t.Status == Deleted {
		return Tenant{}, ErrNotFound
	}
	if t.Reserved {
		return Tenant{}, invalid("the operator tenant cannot be suspended")
	}
	if t.Status == Offboarding {
		return Tenant{}, invalid("the tenant is being removed")
	}
	if !oneLine(reason, 200) {
		return Tenant{}, invalid("the reason must be one line of at most 200 characters")
	}
	switch status {
	case Suspended:
		if t.Status == Suspended {
			return t, nil
		}
		t.Status, t.SuspendedReason, t.SuspendedAt = Suspended, strings.TrimSpace(reason), s.Now().UTC()
	case Active:
		t.Status, t.SuspendedReason, t.SuspendedAt = Active, "", time.Time{}
	default:
		return Tenant{}, invalid("status must be suspended or active")
	}
	return t, s.put(t)
}

// SetSupportAccess is the tenant's own decision about whether an operator may come in.
func (s *Service) SetSupportAccess(id, mode string) (Tenant, error) {
	t, ok := s.load(id)
	if !ok || t.Status == Deleted {
		return Tenant{}, ErrNotFound
	}
	switch mode {
	case SupportOff, SupportAsk, SupportAllow:
	default:
		return Tenant{}, invalid("support access must be off, ask or allow")
	}
	t.SupportAccess = mode
	if mode == SupportOff { // switching it off ends any access granted before
		s.Revoke(id)
	}
	return t, s.put(t)
}

// Ensure makes records for tenants that only exist as a name on users, keys or data, so that an installation that was
// running before this feature appears in the console. It is safe to run again. Returns how many were made.
func (s *Service) Ensure(ids []string) int {
	n := 0
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if _, ok := s.load(id); ok {
			continue
		}
		now := s.Now().UTC()
		t := Tenant{ID: id, Name: id, Status: Active, Created: now, SupportAccess: SupportAllow, SuspendIngest: "reject", Migrated: true}
		if id == perm.OperatorTenant {
			t.Name, t.Reserved, t.SupportAccess = "Operator", true, SupportOff
		}
		c, cancel := ctx5()
		err := s.DB.Create(c, coll, id, t)
		cancel()
		if err == nil {
			n++
		}
		s.invalidate(id)
	}
	return n
}

// Known are the collections whose documents carry a tenant, in the order they are listed when looking for tenants.
var Known = []string{"users", "keys", "groups", "dashboards", "hosts", "instances", "alert_rules", "alert_channels"}

// Discover finds the tenants that appear in stored documents.
func Discover(db docstore.Backend) []string {
	set := map[string]bool{}
	for _, name := range Known {
		c, cancel := ctx5()
		docs, _ := db.List(c, name, nil, 100000)
		cancel()
		for _, d := range docs {
			var v struct {
				Tenant string `json:"tenant"`
			}
			if d.Decode(&v) == nil && v.Tenant != "" {
				set[v.Tenant] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Count tells how many documents of a collection a tenant has (users, keys, groups, dashboards, instances).
func (s *Service) Count(tenant, collection string) int {
	c, cancel := ctx5()
	defer cancel()
	docs, _ := s.DB.List(c, collection, map[string]string{"tenant": tenant}, 100000)
	return len(docs)
}

// ---- support access granted by the tenant ----

// Grant is the tenant's permission for an operator to come in, for a while.
type Grant struct {
	Tenant  string    `json:"tenant"`
	Until   time.Time `json:"until"`
	By      string    `json:"by"`
	Created time.Time `json:"created"`
}

// MaxGrant is the longest a tenant can allow access in one go.
const MaxGrant = 7 * 24 * time.Hour

func (s *Service) GrantAccess(tenant, by string, d time.Duration) (Grant, error) {
	if d < 5*time.Minute || d > MaxGrant {
		return Grant{}, invalid("access can be granted for 5 minutes up to 7 days")
	}
	if t, ok := s.Get(tenant); !ok || t.Status == Deleted {
		return Grant{}, ErrNotFound
	}
	now := s.Now().UTC()
	g := Grant{Tenant: tenant, Until: now.Add(d), By: by, Created: now}
	c, cancel := ctx5()
	defer cancel()
	return g, s.DB.Put(c, grantsColl, tenant, g, "")
}

func (s *Service) Revoke(tenant string) {
	c, cancel := ctx5()
	defer cancel()
	_ = s.DB.Delete(c, grantsColl, tenant)
}

// ActiveGrant is the grant in force, if any.
func (s *Service) ActiveGrant(tenant string) (Grant, bool) {
	c, cancel := ctx5()
	defer cancel()
	d, err := s.DB.Get(c, grantsColl, tenant)
	if err != nil {
		return Grant{}, false
	}
	var g Grant
	if d.Decode(&g) != nil || !s.Now().Before(g.Until) {
		return Grant{}, false
	}
	return g, true
}

// CanEnter says whether an operator may go into the tenant now, and why not if not.
func (s *Service) CanEnter(tenant string) error {
	t, ok := s.load(tenant)
	if !ok || t.Status == Deleted {
		return ErrNotFound
	}
	if t.Status == Offboarding {
		return invalid("the tenant is being removed")
	}
	switch t.SupportAccess {
	case SupportAllow:
		return nil
	case SupportAsk:
		if _, ok := s.ActiveGrant(tenant); ok {
			return nil
		}
		return invalid("this tenant lets an operator in only when one of its administrators has granted access (Settings, Support access). Ask them to grant it")
	}
	return invalid("this tenant has switched support access off")
}

// ---- usage against limits ----

// Usage is what a tenant uses now, to compare with its limits.
type Usage struct {
	Hosts, Instances, Users, Groups, Keys, Dashboards int
	IngestBytesToday                                  int64
}

// Warnings describes the limits that are reached (90 percent) or exceeded, in words a person can act on.
func (q Quotas) Warnings(u Usage) []string {
	var w []string
	check := func(what string, n, limit int) {
		switch {
		case limit <= 0:
		case n > limit:
			w = append(w, fmt.Sprintf("%d %s are in use, but the limit is %d.", n, what, limit))
		case n*10 >= limit*9:
			w = append(w, fmt.Sprintf("%d of %d %s are in use.", n, limit, what))
		}
	}
	check("hosts", u.Hosts, q.Hosts)
	check("Nextcloud instances", u.Instances, q.Instances)
	check("users", u.Users, q.Users)
	check("groups", u.Groups, q.Groups)
	check("agent keys", u.Keys, q.Keys)
	check("dashboards", u.Dashboards, q.Dashboards)
	if q.IngestBytesPerDay > 0 {
		switch {
		case u.IngestBytesToday > q.IngestBytesPerDay:
			w = append(w, fmt.Sprintf("today's data volume (%s) is over the daily limit (%s).", HumanBytes(u.IngestBytesToday), HumanBytes(q.IngestBytesPerDay)))
		case u.IngestBytesToday*10 >= q.IngestBytesPerDay*9:
			w = append(w, fmt.Sprintf("today's data volume is %s of %s.", HumanBytes(u.IngestBytesToday), HumanBytes(q.IngestBytesPerDay)))
		}
	}
	return w
}

// HumanBytes writes a size the way people read it.
func HumanBytes(n int64) string {
	const k = 1024
	if n < k {
		return fmt.Sprintf("%d B", n)
	}
	f, u := float64(n), []string{"KB", "MB", "GB", "TB", "PB"}
	i := -1
	for f >= k && i < len(u)-1 {
		f /= k
		i++
	}
	return fmt.Sprintf("%.3g %s", f, u[i])
}
