package server

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/danielingemar/lumen/internal/billing"
	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/perm"
)

// Billing is what the invoice basis needs: the prices, the recorded usage, and a way to bring today's numbers in.
type Billing struct {
	Svc *billing.Service
	DB  docstore.Backend
	Rec *billing.Recorder // nil: the telemetry store cannot tell usage (then only what was recorded earlier is shown)
	Now func() time.Time
}

func (s *Server) WithBilling(b *Billing) *Server { s.billing = b; return s }

func (b *Billing) now() time.Time {
	if b.Now != nil {
		return b.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Server) billingRoutes(mux *http.ServeMux) {
	if s.billing == nil {
		return
	}
	mux.Handle("GET /api/v1/billing", s.need(perm.Billing, false, s.getBilling))
	mux.Handle("PUT /api/v1/billing", s.need(perm.Billing, true, s.putBilling))
	mux.Handle("GET /api/v1/billing/report", s.need(perm.Billing, false, s.getBillingReport))
}

type billingInstance struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	Registered bool   `json:"registered"`
	HasLogin   bool   `json:"has_login"`     // an administrator login is saved: the data used can be read
	DataDays   int    `json:"data_days"`     // days of this month with the data used
	UsersDays  int    `json:"users_days"`    // days of this month with the number of users
	LastSeen   string `json:"last_reported"` // the latest day with anything
}

// months of the year to pick from: the running one and the eleven before it
func recentPeriods(now time.Time) []string {
	out := make([]string, 0, 12)
	t := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 12; i++ {
		out = append(out, t.AddDate(0, -i, 0).Format("2006-01"))
	}
	return out
}

func (s *Server) knownInstances(tenant string) []billing.Known {
	var out []billing.Known
	for _, i := range s.reg.ListInstances(tenant) {
		if k := i.Out().Key; k != "" {
			out = append(out, billing.Known{Key: k, Name: i.Name})
		}
	}
	return out
}

func (s *Server) getBilling(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	b := s.billing
	cfg := b.Svc.Get(id.Tenant)
	now := b.now()
	cur := now.Format("2006-01")
	if b.Rec != nil {
		b.Rec.Refresh(r.Context(), id.Tenant)
	}
	months, _ := billing.Months(r.Context(), b.DB, id.Tenant, cur)
	seen := map[string]billing.Month{}
	for _, m := range months {
		seen[m.Instance] = m
	}
	byKey := map[string]*billingInstance{}
	for _, i := range s.reg.ListInstances(id.Tenant) {
		out := i.Out()
		if out.Key == "" {
			continue
		}
		byKey[out.Key] = &billingInstance{Key: out.Key, Name: i.Name, Registered: true, HasLogin: out.HasPassword}
	}
	for k, m := range seen {
		bi := byKey[k]
		if bi == nil {
			bi = &billingInstance{Key: k, Name: k}
			byKey[k] = bi
		}
		for day, u := range m.Days {
			if u.Bytes != nil {
				bi.DataDays++
			}
			if u.Users != nil || u.Accounts != nil {
				bi.UsersDays++
			}
			if day > bi.LastSeen {
				bi.LastSeen = day
			}
		}
	}
	list := make([]billingInstance, 0, len(byKey))
	for _, v := range byKey {
		list = append(list, *v)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	writeJSON(w, map[string]any{"config": cfg, "currencies": billing.Currencies, "instances": list, "periods": recentPeriods(now), "today": now.Format("2006-01-02")})
}

func (s *Server) putBilling(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	var in billing.Config
	if !readJSONLimit(w, r, &in, 2<<20) {
		return
	}
	cfg, err := s.billing.Svc.Put(id.Tenant, in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"config": cfg})
}

func (s *Server) getBillingReport(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	b := s.billing
	now := b.now()
	q := r.URL.Query()
	from, to, label := q.Get("from"), q.Get("to"), ""
	if from == "" && to == "" { // a calendar month, as before
		period := q.Get("period")
		if period == "" {
			period = now.Format("2006-01")
		}
		first, n, err := billing.ParsePeriod(period)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		from, to, label = first.Format("2006-01-02"), first.AddDate(0, 0, n-1).Format("2006-01-02"), period
	}
	f, t, err := billing.ParseRange(from, to)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if b.Rec != nil && !t.Before(now.AddDate(0, 0, -45)) { // today's numbers are in a period that reaches the last weeks
		b.Rec.Refresh(r.Context(), id.Tenant)
	}
	months, err := billing.MonthsIn(r.Context(), b.DB, id.Tenant, f, t)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "the recorded usage could not be read: "+err.Error())
		return
	}
	rep, err := billing.BuildRange(b.Svc.Get(id.Tenant), from, to, months, s.knownInstances(id.Tenant), now)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if label != "" {
		rep.Period = label
	} else {
		label = from + "_" + to
	}
	if r.URL.Query().Get("format") == "csv" {
		view := r.URL.Query().Get("view")
		if view != "invoices" {
			view = "lines"
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="lumen-billing-%s-%s.csv"`, label, view))
		w.Header().Set("Cache-Control", "no-store")
		billing.WriteCSV(w, rep, view, r.URL.Query().Get("locale"))
		return
	}
	writeJSON(w, rep)
}
