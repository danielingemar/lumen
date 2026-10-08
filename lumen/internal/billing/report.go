package billing

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Known is an instance that exists, so that one that reported nothing in the month is still listed (with a reason).
type Known struct{ Key, Name string }

// Line is one instance on an invoice. Amounts are minor units of the invoice's currency.
type Line struct {
	Key          string   `json:"key"`
	Name         string   `json:"name"`
	Users        float64  `json:"users"`
	UsersFrom    string   `json:"users_from"` // "server information" | "account list" | ""
	GB           float64  `json:"gb"`
	DaysReported int      `json:"days_reported"`
	UserPrice    string   `json:"user_price"`
	GBPrice      string   `json:"gb_price"`
	UserAmount   int64    `json:"user_amount"`
	DataAmount   int64    `json:"data_amount"`
	Gross        int64    `json:"gross"`
	DiscountPct  string   `json:"discount_percent"`
	Discount     int64    `json:"discount"`
	Net          int64    `json:"net"`
	VATPercent   string   `json:"vat_percent"`
	Note         string   `json:"note,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
}

// VATLine is the VAT of one rate on an invoice, which is how an invoice states it.
type VATLine struct {
	Percent string `json:"percent"`
	Base    int64  `json:"base"`
	VAT     int64  `json:"vat"`
}

// Invoice is the basis for one invoice: one customer, one currency.
type Invoice struct {
	Customer string    `json:"customer"`
	Currency string    `json:"currency"`
	Decimals int       `json:"decimals"`
	Lines    []Line    `json:"lines"`
	Net      int64     `json:"net"`
	VATLines []VATLine `json:"vat_lines"`
	VAT      int64     `json:"vat"`
	Total    int64     `json:"total"`
}

// Total is the sum of the invoices of one currency.
type Total struct {
	Currency string `json:"currency"`
	Decimals int    `json:"decimals"`
	Net      int64  `json:"net"`
	VAT      int64  `json:"vat"`
	Total    int64  `json:"total"`
}

// Report is the basis for a month's invoices.
type Report struct {
	Period    string    `json:"period"`   // 2026-09
	From      string    `json:"from"`     // 2026-09-01
	To        string    `json:"to"`       // 2026-09-30
	Days      int       `json:"days"`     // days in the month
	Complete  bool      `json:"complete"` // the month is over
	UserBasis string    `json:"user_basis"`
	DataBasis string    `json:"data_basis"`
	GBUnit    string    `json:"gb_unit"`
	Prorate   bool      `json:"prorate"`
	Invoices  []Invoice `json:"invoices"`
	Totals    []Total   `json:"totals"`
	Excluded  int       `json:"excluded"`
	Warnings  []string  `json:"warnings,omitempty"` // about the report as a whole
	Generated string    `json:"generated"`
}

// ParsePeriod reads "2026-09" and returns the first day and the number of days of that month.
func ParsePeriod(p string) (time.Time, int, error) {
	t, err := time.Parse("2006-01", p)
	if err != nil {
		return t, 0, fmt.Errorf("the period must look like 2026-09")
	}
	return t, time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day(), nil
}

type agg struct {
	n               int
	sum, max, last  float64
	lastDay, source string
}

func (a *agg) add(day string, v float64) {
	a.n++
	a.sum += v
	if v > a.max || a.n == 1 {
		a.max = v
	}
	if day >= a.lastDay {
		a.last, a.lastDay = v, day
	}
}

func (a *agg) value(basis string) float64 {
	if a.n == 0 {
		return 0
	}
	switch basis {
	case Average:
		return a.sum / float64(a.n)
	case End:
		return a.last
	}
	return a.max
}

// Build makes the report of one month from the prices, the recorded days and the instances that exist.
func Build(cfg Config, period string, months []Month, known []Known, now time.Time) (Report, error) {
	first, nDays, err := ParsePeriod(period)
	if err != nil {
		return Report{}, err
	}
	cfg, err = cfg.Clean()
	if err != nil {
		return Report{}, err
	}
	last := first.AddDate(0, 0, nDays-1)
	rep := Report{Period: period, From: first.Format("2006-01-02"), To: last.Format("2006-01-02"), Days: nDays,
		Complete:  now.UTC().After(last.AddDate(0, 0, 1)) || now.UTC().Equal(last.AddDate(0, 0, 1)),
		UserBasis: cfg.UserBasis, DataBasis: cfg.DataBasis, GBUnit: cfg.GBUnit, Prorate: cfg.Prorate, Invoices: []Invoice{}, Totals: []Total{},
		Generated: now.UTC().Format(time.RFC3339)}
	if !rep.Complete {
		rep.Warnings = append(rep.Warnings, "The month is not over: this is the basis so far, and it changes until the last day.")
	}
	unit := 1e9
	if cfg.GBUnit == "gib" {
		unit = 1 << 30
	}
	names := map[string]string{}
	keys := map[string]bool{}
	for _, k := range known {
		names[k.Key] = k.Name
		keys[k.Key] = true
	}
	byKey := map[string]Month{}
	for _, m := range months {
		byKey[m.Instance] = m
		keys[m.Instance] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	type ik struct{ customer, currency string }
	invoices := map[ik]*Invoice{}
	var order []ik
	anyData := false
	for _, key := range sorted {
		p := cfg.Profile(key)
		if p.Exclude {
			rep.Excluded++
			continue
		}
		name := names[key]
		if name == "" {
			name = key
		}
		customer := p.Customer
		if customer == "" {
			customer = name
		}
		cur := cfg.Currency
		userPrice, gbPrice := cfg.UserPrice, cfg.GBPrice
		if p.Currency != "" {
			cur = p.Currency
		}
		if p.UserPrice != "" {
			userPrice = p.UserPrice
		}
		if p.GBPrice != "" {
			gbPrice = p.GBPrice
		}
		c, _ := Lookup(cur)
		up, _ := ParseDec(userPrice)
		gp, _ := ParseDec(gbPrice)
		vat := cfg.VAT
		if p.VAT != "" {
			vat = p.VAT
		}
		vatD, _ := ParseDec(vat)
		discD, _ := ParseDec(p.Discount)

		var users, accounts, bytes agg
		for day, u := range byKey[key].Days {
			if day < rep.From || day > rep.To {
				continue
			}
			if u.Users != nil {
				users.add(day, *u.Users)
			}
			if u.Accounts != nil {
				accounts.add(day, *u.Accounts)
			}
			if u.Bytes != nil {
				bytes.add(day, *u.Bytes)
			}
		}
		days := map[string]bool{}
		for day, u := range byKey[key].Days {
			if day >= rep.From && day <= rep.To && (u.Users != nil || u.Accounts != nil || u.Bytes != nil) {
				days[day] = true
			}
		}
		ln := Line{Key: key, Name: name, DaysReported: len(days), UserPrice: up.String(), GBPrice: gp.String(), DiscountPct: discD.String(),
			VATPercent: vatD.String(), Note: p.Note}
		ua := users
		switch {
		case users.n > 0:
			ln.UsersFrom = "server information"
		case accounts.n > 0:
			ua, ln.UsersFrom = accounts, "account list"
		}
		ln.Users = round3(ua.value(cfg.UserBasis))
		ln.GB = round3(bytes.value(cfg.DataBasis) / unit)
		if len(days) == 0 {
			ln.Warnings = append(ln.Warnings, "Nothing was reported this month, so nothing is charged. Is the agent running, and is the instance added?")
		} else {
			anyData = true
			if ua.n == 0 {
				ln.Warnings = append(ln.Warnings, "The number of users is not reported (needs a serverinfo token or an administrator login on the instance).")
			}
			if bytes.n == 0 {
				ln.Warnings = append(ln.Warnings, "The data used is not reported: it needs an administrator login (user and app password) on the instance, with the Provisioning API enabled.")
			}
			if !cfg.Prorate && len(days) < nDays && rep.Complete {
				ln.Warnings = append(ln.Warnings, fmt.Sprintf("Reported %d of %d days but is charged for the whole month (turn on 'charge only the days reported' to change that).", len(days), nDays))
			}
		}
		num, den := int64(1), int64(1)
		if cfg.Prorate && len(days) > 0 {
			num, den = int64(len(days)), int64(nDays)
		}
		ln.UserAmount = amount(milli(ln.Users), up, c.Decimals, num, den)
		ln.DataAmount = amount(milli(ln.GB), gp, c.Decimals, num, den)
		ln.Gross = ln.UserAmount + ln.DataAmount
		ln.Discount = percentOf(ln.Gross, discD)
		ln.Net = ln.Gross - ln.Discount

		k := ik{customer, cur}
		inv := invoices[k]
		if inv == nil {
			inv = &Invoice{Customer: customer, Currency: cur, Decimals: c.Decimals, Lines: []Line{}}
			invoices[k] = inv
			order = append(order, k)
		}
		inv.Lines = append(inv.Lines, ln)
	}
	if !anyData && len(sorted) > 0 {
		rep.Warnings = append(rep.Warnings, "No instance has reported anything for this month.")
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].customer != order[j].customer {
			return strings.ToLower(order[i].customer) < strings.ToLower(order[j].customer)
		}
		return order[i].currency < order[j].currency
	})
	totals := map[string]*Total{}
	var tcur []string
	for _, k := range order {
		inv := invoices[k]
		base := map[string]int64{}
		var rates []string
		for _, l := range inv.Lines {
			inv.Net += l.Net
			if _, ok := base[l.VATPercent]; !ok {
				rates = append(rates, l.VATPercent)
			}
			base[l.VATPercent] += l.Net
		}
		sort.Strings(rates)
		for _, r := range rates {
			d, _ := ParseDec(r)
			v := percentOf(base[r], d)
			inv.VATLines = append(inv.VATLines, VATLine{Percent: r, Base: base[r], VAT: v})
			inv.VAT += v
		}
		inv.Total = inv.Net + inv.VAT
		rep.Invoices = append(rep.Invoices, *inv)
		t := totals[inv.Currency]
		if t == nil {
			t = &Total{Currency: inv.Currency, Decimals: inv.Decimals}
			totals[inv.Currency] = t
			tcur = append(tcur, inv.Currency)
		}
		t.Net += inv.Net
		t.VAT += inv.VAT
		t.Total += inv.Total
	}
	sort.Strings(tcur)
	for _, c := range tcur {
		rep.Totals = append(rep.Totals, *totals[c])
	}
	if cfg.UserPrice == "0.00" && cfg.GBPrice == "0.00" && len(cfg.Profiles) == 0 {
		rep.Warnings = append(rep.Warnings, "No prices are set yet, so every amount is zero.")
	}
	return rep, nil
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }
func milli(f float64) int64    { return int64(math.Round(f * 1000)) }
