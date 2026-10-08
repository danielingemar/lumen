package billing

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
)

const usageColl = "billing_usage"

// DayUsage is what one instance used on one UTC day: the highest number seen. A nil field was not reported that day.
type DayUsage struct {
	Users    *float64 `json:"users,omitempty"`    // from Nextcloud's server information
	Accounts *float64 `json:"accounts,omitempty"` // enabled accounts, from the account list (used when the other is missing)
	Bytes    *float64 `json:"bytes,omitempty"`    // what the accounts' files take, from the account list
}

// Month is one instance's days of one month. It is kept for as long as the operator keeps the installation, so an invoice
// can be made long after the telemetry itself has expired (30 days by default).
type Month struct {
	Tenant   string              `json:"tenant"`
	Instance string              `json:"instance"`
	Month    string              `json:"month"` // 2026-10
	Days     map[string]DayUsage `json:"days"`  // 2026-10-08
	Updated  time.Time           `json:"updated"`
}

func monthID(tenant, instance, month string) string { return tenant + "|" + instance + "|" + month }

// Sample is one day of one instance as the database of telemetry answers.
type Sample struct {
	Instance string
	Day      string
	DayUsage
}

// Source is where the numbers come from: the day-by-day maximum of the instance metrics.
type Source interface {
	InstanceUsage(ctx context.Context, tenant string, from, to time.Time) ([]Sample, error)
}

// Recorder copies the numbers from the telemetry into Months, so that they outlive it.
type Recorder struct {
	DB      docstore.Backend
	Src     Source
	Tenants func() []string
	Log     *slog.Logger
	Now     func() time.Time
	// Backfill is how far back the first pass looks (the retention of the telemetry); every later pass looks at two days.
	Backfill time.Duration

	mu    sync.Mutex
	done  bool
	fresh map[string]time.Time // tenant -> when it was last refreshed on request
}

func (r *Recorder) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func dayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// Run records every half hour until ctx ends.
func (r *Recorder) Run(ctx context.Context) {
	for {
		r.Pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Minute):
		}
	}
}

// Pass records for every tenant. The first pass reaches back as far as the telemetry does.
func (r *Recorder) Pass(ctx context.Context) {
	r.mu.Lock()
	first := !r.done
	r.done = true
	r.mu.Unlock()
	back := 48 * time.Hour
	if first && r.Backfill > back {
		back = r.Backfill
	}
	for _, t := range r.Tenants() {
		if err := r.Record(ctx, t, back); err != nil && r.Log != nil {
			r.Log.Warn("recording the usage of the instances failed", "tenant", t, "err", err)
		}
	}
}

// Refresh records one tenant's last two days, at most once a minute. A report of the running month calls it, so that
// today's numbers are in it.
func (r *Recorder) Refresh(ctx context.Context, tenant string) {
	r.mu.Lock()
	if r.fresh == nil {
		r.fresh = map[string]time.Time{}
	}
	if t, ok := r.fresh[tenant]; ok && r.now().Sub(t) < time.Minute {
		r.mu.Unlock()
		return
	}
	r.fresh[tenant] = r.now()
	r.mu.Unlock()
	if err := r.Record(ctx, tenant, 48*time.Hour); err != nil && r.Log != nil {
		r.Log.Warn("recording the usage of the instances failed", "tenant", tenant, "err", err)
	}
}

// Record reads the last `back` of one tenant (from the start of a UTC day, so that a day is always read whole) and
// merges it into the stored months. A day that is read again is replaced, not added to.
func (r *Recorder) Record(ctx context.Context, tenant string, back time.Duration) error {
	now := r.now()
	from := dayStart(now.Add(-back))
	samples, err := r.Src.InstanceUsage(ctx, tenant, from, now.Add(time.Minute))
	if err != nil {
		return err
	}
	type k struct{ inst, month string }
	groups := map[k][]Sample{}
	for _, s := range samples {
		if s.Instance == "" || len(s.Day) != 10 {
			continue
		}
		groups[k{s.Instance, s.Day[:7]}] = append(groups[k{s.Instance, s.Day[:7]}], s)
	}
	for g, list := range groups {
		id := monthID(tenant, g.inst, g.month)
		m := Month{Tenant: tenant, Instance: g.inst, Month: g.month, Days: map[string]DayUsage{}}
		if doc, err := r.DB.Get(ctx, usageColl, id); err == nil {
			var old Month
			if doc.Decode(&old) == nil && old.Days != nil {
				m.Days = old.Days
			}
		}
		before := map[string]DayUsage{}
		for d, v := range m.Days {
			before[d] = v
		}
		for _, s := range list {
			m.Days[s.Day] = s.DayUsage
		}
		if reflect.DeepEqual(before, m.Days) {
			continue
		}
		m.Updated = now
		if err := r.DB.Put(ctx, usageColl, id, m, ""); err != nil {
			return err
		}
	}
	return nil
}

// Months returns what is recorded for a month ("2026-10") of one tenant.
func Months(ctx context.Context, db docstore.Backend, tenant, month string) ([]Month, error) {
	docs, err := db.List(ctx, usageColl, map[string]string{"tenant": tenant, "month": month}, 10000)
	if err != nil {
		return nil, err
	}
	out := make([]Month, 0, len(docs))
	for _, d := range docs {
		var m Month
		if d.Decode(&m) == nil && m.Tenant == tenant && m.Month == month {
			out = append(out, m)
		}
	}
	return out, nil
}
