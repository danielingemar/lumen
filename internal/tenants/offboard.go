package tenants

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
)

// Purger removes a tenant's telemetry from the shared tables and reports what is left (the telemetry store implements it).
type Purger interface {
	PurgeTenant(ctx context.Context, tenant string) error
	TenantRows(ctx context.Context, tenant string) (int64, error)
}

// PurgeCollections are the document collections that are emptied for a tenant when it is removed. Two things are kept on
// purpose: what the tenant used (the usage records, for the operator's invoices) and the audit log.
var PurgeCollections = []string{"users", "keys", "groups", "dashboards", "hosts", "instances", "alert_rules", "alert_state", "alert_events", "alert_silences", "alert_channels", "support_grants"}

// Offboarder removes a tenant: it empties the document store, the telemetry and the backups, and checks that nothing is left.
type Offboarder struct {
	Svc       *Service
	Purger    Purger
	BackupDir string
	Log       *slog.Logger
}

func (o *Offboarder) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

var stepNames = []string{"documents", "telemetry", "backups", "check"}

// Begin marks a tenant as being removed. confirm must be the tenant's id (so that it is never done by a stray click).
func (o *Offboarder) Begin(id, confirm string) error {
	t, ok := o.Svc.load(id)
	if !ok || t.Status == Deleted {
		return ErrNotFound
	}
	if t.Reserved {
		return invalid("the operator tenant cannot be removed")
	}
	if confirm != id {
		return invalid("to remove a tenant, give its id (%s) as confirmation", id)
	}
	if t.Status == Offboarding && t.Offboard != nil && t.Offboard.State == "purging" {
		return invalid("the tenant is already being removed")
	}
	t.Status = Offboarding
	t.Offboard = &Offboard{State: "purging", Started: o.Svc.Now().UTC()}
	for _, n := range stepNames {
		t.Offboard.Steps = append(t.Offboard.Steps, Step{Name: n})
	}
	return o.Svc.put(t)
}

// Start begins the removal and runs it in the background.
func (o *Offboarder) Start(id, confirm string) error {
	if err := o.Begin(id, confirm); err != nil {
		return err
	}
	go o.Run(context.Background(), id)
	return nil
}

func (o *Offboarder) record(id string, mutate func(t *Tenant)) {
	if t, ok := o.Svc.load(id); ok {
		mutate(&t)
		_ = o.Svc.put(t)
	}
}

func (o *Offboarder) fail(id string, step int, err error) {
	o.log().Error("offboarding failed", "tenant", id, "step", stepNames[step], "err", err)
	o.record(id, func(t *Tenant) {
		t.Offboard.State, t.Offboard.Error, t.Offboard.Finished = "failed", fmt.Sprintf("%s: %v", stepNames[step], err), o.Svc.Now().UTC()
	})
}

func (o *Offboarder) done(id string, step int, detail string) {
	o.record(id, func(t *Tenant) { t.Offboard.Steps[step].Done, t.Offboard.Steps[step].Detail = true, detail })
}

// remaining counts the documents a tenant still has in the purged collections.
func (o *Offboarder) remaining(id string) (int, []string) {
	n := 0
	var where []string
	for _, c := range PurgeCollections {
		if k := o.Svc.Count(id, c); k > 0 {
			n += k
			where = append(where, fmt.Sprintf("%s: %d", c, k))
		}
	}
	return n, where
}

// Run does the work. It can be run again after a failure.
func (o *Offboarder) Run(ctx context.Context, id string) {
	o.log().Info("offboarding started", "tenant", id)
	// 1. documents
	total, colls := 0, 0
	for _, name := range PurgeCollections {
		c, cancel := ctx5()
		docs, err := o.Svc.DB.List(c, name, map[string]string{"tenant": id}, 100000)
		cancel()
		if err != nil {
			o.fail(id, 0, err)
			return
		}
		if len(docs) > 0 {
			colls++
		}
		for _, d := range docs {
			c, cancel := ctx5()
			err := o.Svc.DB.Delete(c, name, d.ID)
			cancel()
			if err != nil && err != docstore.ErrNotFound {
				o.fail(id, 0, err)
				return
			}
			total++
		}
	}
	o.done(id, 0, fmt.Sprintf("%d documents in %d collections removed (users, keys, dashboards, hosts, instances, alert rules and channels)", total, colls))
	// 2. telemetry
	if o.Purger != nil {
		if err := o.Purger.PurgeTenant(ctx, id); err != nil {
			o.fail(id, 1, err)
			return
		}
		o.done(id, 1, "traces, logs and metrics removed from the shared tables, including archived days")
	} else {
		o.done(id, 1, "no telemetry store is connected")
	}
	// 3. backups
	detail := "no backup directory is configured"
	if o.BackupDir != "" {
		if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
			detail = "the tenant's name cannot be used as a directory name, so its backups were not touched; remove them by hand"
		} else {
			matches, _ := filepath.Glob(filepath.Join(o.BackupDir, "telemetry", "*", id))
			for _, m := range matches {
				if err := os.RemoveAll(m); err != nil {
					o.fail(id, 2, err)
					return
				}
			}
			detail = fmt.Sprintf("%d daily backup directories removed. The configuration dumps in %s/config contain the tenant's settings until they are replaced (the newest 30 are kept)", len(matches), o.BackupDir)
		}
	}
	o.done(id, 2, detail)
	// 4. check that nothing is left
	if n, where := o.remaining(id); n > 0 {
		o.fail(id, 3, fmt.Errorf("%d documents are still there (%s); run the removal again", n, strings.Join(where, ", ")))
		return
	}
	if o.Purger != nil {
		rows, err := o.Purger.TenantRows(ctx, id)
		if err != nil {
			o.fail(id, 3, err)
			return
		}
		if rows > 0 {
			o.fail(id, 3, fmt.Errorf("%d telemetry rows are still there; data may still be arriving for this tenant. Stop its agents and run the removal again", rows))
			return
		}
	}
	o.done(id, 3, "nothing is left for this tenant")
	o.record(id, func(t *Tenant) {
		now := o.Svc.Now().UTC()
		t.Status, t.Deleted, t.Contact, t.Notes = Deleted, now, "", "" // the name stays taken; personal details do not
		t.Offboard.State, t.Offboard.Finished, t.Offboard.Error = "done", now, ""
	})
	o.log().Info("offboarding done", "tenant", id)
}

// ---- export ----

// secretKeys are never put in an export: the hashes of passwords and keys, and sealed secrets.
var secretKeys = map[string]bool{"hash": true, "token_enc": true, "password_enc": true, "secrets_enc": true, "secret": true}

func strip(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if secretKeys[strings.ToLower(k)] {
				delete(x, k)
				continue
			}
			x[k] = strip(val)
		}
	case []any:
		for i := range x {
			x[i] = strip(x[i])
		}
	}
	return v
}

// ExportCollections are what goes into a tenant's export.
var ExportCollections = append(append([]string{}, PurgeCollections...), "usage", "audit")

// Export writes a zip with the tenant's documents as JSON files. Passwords, key hashes and sealed secrets are left out.
// Telemetry is not in it: that is in the daily backups.
func (s *Service) Export(w io.Writer, id string) error {
	t, ok := s.load(id)
	if !ok || t.Status == Deleted {
		return ErrNotFound
	}
	zw := zip.NewWriter(w)
	add := func(name string, v any) error {
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	if err := add("tenant.json", t); err != nil {
		return err
	}
	for _, name := range ExportCollections {
		c, cancel := ctx5()
		docs, err := s.DB.List(c, name, map[string]string{"tenant": id}, 100000)
		cancel()
		if err != nil {
			return err
		}
		items := make([]any, 0, len(docs))
		for _, d := range docs {
			var m any
			if json.Unmarshal(d.Data, &m) == nil {
				items = append(items, strip(m))
			}
		}
		sort.SliceStable(items, func(i, j int) bool { return fmt.Sprint(items[i]) < fmt.Sprint(items[j]) })
		if len(items) > 0 {
			if err := add(name+".json", items); err != nil {
				return err
			}
		}
	}
	f, err := zw.Create("README.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(f, "Export of the tenant %q from Lumen, made %s.\n\nIt contains the tenant's settings and documents as JSON: users (without passwords), groups, dashboards, hosts, instances, alert rules, silences and channels (without secrets), usage and the access log.\nIt does not contain traces, logs and metrics: those are in the daily backups (one directory per day and tenant, JSON lines compressed with gzip).\nPasswords, API-key hashes and stored tokens are never exported.\n", id, s.Now().UTC().Format(time.RFC3339))
	return zw.Close()
}
