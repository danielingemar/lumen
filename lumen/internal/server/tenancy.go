package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/danielingemar/lumen/internal/audit"
	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/license"
	"github.com/danielingemar/lumen/internal/metering"
	"github.com/danielingemar/lumen/internal/perm"
	"github.com/danielingemar/lumen/internal/tenants"
)

// Tenancy is everything about tenants as a whole: their records, what they use, the record of who did what, and the
// removal of a tenant. The tenant console, limits and suspension belong to the Operator add-on (they do nothing without a
// licence), but counting what each tenant sends and keeping a record per tenant always runs, so that the numbers exist
// from the first day.
type Tenancy struct {
	AuditDays int // how long entries of the audit log are kept (0 = until there are more than 10 000 per tenant)
	Tenants   *tenants.Service
	Meter     *metering.Meter
	Limiter   *metering.Limiter
	Audit     *audit.Log
	Off       *tenants.Offboarder
}

// WithTenancy enables tenant records, metering, limits and the operator console.
func (s *Server) WithTenancy(t *Tenancy) *Server { s.ten = t; return s }

// enforced tells whether limits and suspension apply. They need the Operator licence: without it Lumen only counts.
func (s *Server) enforced() bool {
	return s.ten != nil && s.lic != nil && s.lic.Allows(license.Operator)
}

// objectKinds are the things that can be limited by count, with the collection that holds them and what to call them.
var objectKinds = map[string]struct{ coll, label string }{
	"hosts": {"", "hosts"}, "instances": {"instances", "Nextcloud instances"}, "users": {"users", "users"},
	"groups": {"groups", "groups"}, "keys": {"keys", "agent keys"}, "dashboards": {"dashboards", "dashboards"},
}

func limitFor(q tenants.Quotas, kind string) int {
	switch kind {
	case "hosts":
		return q.Hosts
	case "instances":
		return q.Instances
	case "users":
		return q.Users
	case "groups":
		return q.Groups
	case "keys":
		return q.Keys
	case "dashboards":
		return q.Dashboards
	}
	return 0
}

// Counts is how many of each object a tenant has now.
func (t *Tenancy) Counts(tenant string) map[string]int {
	out := map[string]int{"hosts": t.Meter.ActiveHosts(tenant)}
	for kind, k := range objectKinds {
		if k.coll != "" {
			out[kind] = t.Tenants.Count(tenant, k.coll)
		}
	}
	return out
}

func (t *Tenancy) usage(tenant string, q tenants.Quotas) (tenants.Usage, []string) {
	c := t.Counts(tenant)
	u := tenants.Usage{Hosts: c["hosts"], Instances: c["instances"], Users: c["users"], Groups: c["groups"], Keys: c["keys"], Dashboards: c["dashboards"], IngestBytesToday: t.Meter.BytesToday(tenant)}
	return u, q.Warnings(u)
}

// touch makes sure a tenant that has just appeared (a new API key, a first sign-in) has a record.
func (s *Server) touch(tenant string) {
	if s.ten == nil || tenant == "" {
		return
	}
	if _, ok := s.ten.Tenants.Get(tenant); !ok {
		s.ten.Tenants.Ensure([]string{tenant})
	}
}

// tenantUsable writes the answer and returns false when the tenant may not be used (suspended or being removed).
func (s *Server) tenantUsable(w http.ResponseWriter, id edition.Identity) bool {
	if s.ten == nil || id.Tenant == "" {
		return true
	}
	s.touch(id.Tenant)
	if !s.enforced() || id.Acting {
		return true
	}
	t, ok := s.ten.Tenants.Get(id.Tenant)
	if !ok {
		return true
	}
	switch t.Status {
	case tenants.Suspended:
		msg := "This account is suspended"
		if t.SuspendedReason != "" {
			msg += " (" + t.SuspendedReason + ")"
		}
		writeErr(w, http.StatusForbidden, msg+". Contact your provider.")
		return false
	case tenants.Offboarding, tenants.Deleted:
		writeErr(w, http.StatusForbidden, "This account has been closed. Contact your provider.")
		return false
	}
	return true
}

// quotaOK checks the limit on the number of one kind of object before one more is made.
func (s *Server) quotaOK(w http.ResponseWriter, id edition.Identity, kind string) bool {
	if !s.enforced() || id.Acting {
		return true
	}
	t, ok := s.ten.Tenants.Get(id.Tenant)
	if !ok || !t.Quotas.Hard {
		return true
	}
	limit := limitFor(t.Quotas, kind)
	k := objectKinds[kind]
	if limit <= 0 || k.coll == "" || s.ten.Tenants.Count(id.Tenant, k.coll) < limit {
		return true
	}
	writeErr(w, http.StatusForbidden, fmt.Sprintf("Limit reached: your plan allows %d %s. Ask your provider to raise the limit, or remove one first.", limit, k.label))
	return false
}

// ---- incoming telemetry ----

// admitError is a refusal of a request with the status it should get and, for 429, how long to wait.
type admitError struct {
	code  int
	msg   string
	retry time.Duration
}

func (e *admitError) Error() string { return e.msg }

// errDropped means the tenant is suspended with the "drop" setting: the request is answered as accepted and thrown away.
var errDropped = errors.New("dropped")

// admit decides whether a batch of telemetry may be stored. It is where suspension and the limits on rate, daily volume and
// number of hosts are applied.
func (s *Server) admit(id edition.Identity, items, bytes int, hosts []string) error {
	if s.ten == nil {
		return nil
	}
	s.touch(id.Tenant)
	if !s.enforced() {
		return nil
	}
	t, ok := s.ten.Tenants.Get(id.Tenant)
	if !ok {
		return nil
	}
	switch t.Status {
	case tenants.Suspended:
		if t.SuspendIngest == "drop" {
			return errDropped
		}
		return &admitError{code: http.StatusForbidden, msg: "this tenant is suspended: incoming data is refused. Contact your provider"}
	case tenants.Offboarding, tenants.Deleted:
		return &admitError{code: http.StatusForbidden, msg: "this tenant has been closed: incoming data is refused"}
	}
	q := t.Quotas
	if !q.Hard {
		return nil
	}
	if q.IngestPerSec > 0 {
		if ok, wait := s.ten.Limiter.Allow(id.Tenant, items, q.IngestPerSec); !ok {
			return &admitError{code: http.StatusTooManyRequests, msg: fmt.Sprintf("the limit of %d items per second is reached; try again shortly", q.IngestPerSec), retry: wait}
		}
	}
	if q.IngestBytesPerDay > 0 && s.ten.Meter.BytesToday(id.Tenant)+int64(bytes) > q.IngestBytesPerDay {
		until := time.Until(time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour))
		if until > time.Hour {
			until = time.Hour
		}
		return &admitError{code: http.StatusTooManyRequests, msg: fmt.Sprintf("the daily data limit (%s) is reached; it starts over at midnight UTC", tenants.HumanBytes(q.IngestBytesPerDay)), retry: until}
	}
	if q.Hosts > 0 && len(hosts) > 0 {
		if n := s.ten.Meter.NewHosts(id.Tenant, hosts); n > 0 && s.ten.Meter.ActiveHosts(id.Tenant)+n > q.Hosts {
			return &admitError{code: http.StatusForbidden, msg: fmt.Sprintf("the limit of %d hosts is reached; a new host cannot be added. Contact your provider", q.Hosts)}
		}
	}
	return nil
}

func (s *Server) metered(id edition.Identity, signal string, items, bytes int, hosts []string) {
	if s.ten != nil {
		s.ten.Meter.Record(id.Tenant, signal, items, bytes, hosts)
	}
}

// retryAfter writes the header for a refusal that says when to come back.
func retryAfter(w http.ResponseWriter, d time.Duration) {
	if d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int((d+time.Second-1)/time.Second)))
	}
}

// ---- the record of what was done ----

func (s *Server) auditOp(id edition.Identity, tenant, action, target, detail string) {
	log := s.auditLog()
	if log == nil {
		return
	}
	who := id.User
	if who == "" {
		who = "api key"
	}
	if err := log.Add(audit.Entry{Tenant: tenant, Actor: who, Acting: id.Acting, Action: action, Target: target, Detail: detail}); err != nil {
		s.log.Error("audit entry could not be written", "action", action, "err", err)
	}
}

// auditActing records what an operator changes while inside a tenant: every request that is not a plain read.
func (s *Server) auditActing(r *http.Request, id edition.Identity) {
	if !id.Acting || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return
	}
	s.auditOp(edition.Identity{User: id.Operator, Acting: true}, id.Tenant, "support."+r.Method, r.URL.Path, "")
}

// ---- background work ----

// Run flushes the counters, samples what each tenant has, and tidies old records, until ctx ends.
func (t *Tenancy) Run(ctx context.Context, log *slog.Logger) {
	flush, sample, tidy := time.NewTicker(time.Minute), time.NewTicker(10*time.Minute), time.NewTicker(6*time.Hour)
	defer flush.Stop()
	defer sample.Stop()
	defer tidy.Stop()
	t.sampleAll()
	for {
		select {
		case <-ctx.Done():
			t.Meter.Flush()
			return
		case <-flush.C:
			t.Meter.Flush()
		case <-sample.C:
			t.sampleAll()
		case <-tidy.C:
			t.Meter.Prune(400)
			t.Audit.Retain(10000, time.Duration(t.AuditDays)*24*time.Hour)
			log.Debug("tenancy tidied")
		}
	}
}

func (t *Tenancy) sampleAll() {
	for _, tn := range t.Tenants.List() {
		if tn.Status == tenants.Deleted {
			continue
		}
		t.Meter.Sample(tn.ID, t.Counts(tn.ID))
	}
}

func ctxShort() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 8*time.Second)
}

var _ = perm.OperatorTenant
