package billing

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
)

const coll = "billing"

// Basis says which number of the month a quantity is charged on.
const (
	Peak    = "peak"    // the highest value of any day: the customer is charged for the most they had
	Average = "average" // the mean of the days that reported
	End     = "end"     // the value on the last day that reported
)

// Config is one tenant's prices and rules. Prices are texts ("49.50") so that nothing is lost on the way through JSON.
type Config struct {
	Currency  string    `json:"currency"`    // what prices are in, unless a customer has its own currency
	UserPrice string    `json:"user_price"`  // per user and month
	GBPrice   string    `json:"gb_price"`    // per gigabyte of stored data and month
	UserBasis string    `json:"user_basis"`  // peak | average | end
	DataBasis string    `json:"data_basis"`  // peak | average | end
	GBUnit    string    `json:"gb_unit"`     // gb (10^9 bytes, what a disk is sold in) | gib (2^30 bytes, what most tools show)
	VAT       string    `json:"vat_percent"` // 0..100
	Prorate   bool      `json:"prorate"`     // an instance that reported fewer days than the month is charged for those days
	Profiles  []Profile `json:"profiles"`    // per instance: who the customer is, and exceptions
}

// Profile is what is special about one instance. Everything but Key is optional.
type Profile struct {
	Key       string `json:"key"`      // the instance (host[:port] of its address)
	Customer  string `json:"customer"` // instances with the same customer are on one invoice; empty: the instance's name
	Exclude   bool   `json:"exclude"`  // not invoiced (your own, a test)
	Currency  string `json:"currency"` // another currency than the default: then both prices below are required
	UserPrice string `json:"user_price"`
	GBPrice   string `json:"gb_price"`
	Discount  string `json:"discount_percent"`
	VAT       string `json:"vat_percent"` // empty: the default (0 for a customer abroad, for example)
	Note      string `json:"note"`
}

var keyRe = regexp.MustCompile(`^[A-Za-z0-9_.:\[\]-]{1,255}$`)

// Default is the configuration before anybody has set anything: nothing is charged.
func Default() Config {
	return Config{Currency: "EUR", UserPrice: "0.00", GBPrice: "0.00", UserBasis: Peak, DataBasis: Peak, GBUnit: "gb", VAT: "0.00", Profiles: []Profile{}}
}

func okBasis(b string) bool { return b == Peak || b == Average || b == End }

// Clean checks a configuration and returns it in its canonical form (prices as "49.50", no stray spaces).
func (c Config) Clean() (Config, error) {
	d := Default()
	if c.Currency == "" {
		c.Currency = d.Currency
	}
	if _, ok := Lookup(c.Currency); !ok {
		return c, fmt.Errorf("the currency %q is not one of the supported ones", c.Currency)
	}
	for _, p := range []struct {
		what string
		v    *string
	}{{"the price per user", &c.UserPrice}, {"the price per GB", &c.GBPrice}} {
		n, err := ParseDec(*p.v)
		if err != nil {
			return c, fmt.Errorf("%s: %w", p.what, err)
		}
		*p.v = n.String()
	}
	vat, err := pct(c.VAT, "the VAT")
	if err != nil {
		return c, err
	}
	c.VAT = vat
	if c.UserBasis == "" {
		c.UserBasis = d.UserBasis
	}
	if c.DataBasis == "" {
		c.DataBasis = d.DataBasis
	}
	if !okBasis(c.UserBasis) || !okBasis(c.DataBasis) {
		return c, fmt.Errorf("the basis must be peak, average or end")
	}
	if c.GBUnit == "" {
		c.GBUnit = d.GBUnit
	}
	if c.GBUnit != "gb" && c.GBUnit != "gib" {
		return c, fmt.Errorf("the unit must be gb or gib")
	}
	if len(c.Profiles) > 5000 {
		return c, fmt.Errorf("at most 5000 instances can have their own settings")
	}
	seen := map[string]bool{}
	out := make([]Profile, 0, len(c.Profiles))
	for _, p := range c.Profiles {
		p.Key = strings.TrimSpace(p.Key)
		if !keyRe.MatchString(p.Key) {
			return c, fmt.Errorf("%q is not an instance", p.Key)
		}
		if seen[p.Key] {
			return c, fmt.Errorf("the instance %s is listed twice", p.Key)
		}
		seen[p.Key] = true
		p.Customer = strings.TrimSpace(p.Customer)
		p.Note = strings.TrimSpace(p.Note)
		if len([]rune(p.Customer)) > 80 || len([]rune(p.Note)) > 300 {
			return c, fmt.Errorf("%s: the customer name may be 80 characters and the note 300", p.Key)
		}
		if p.Currency != "" {
			if _, ok := Lookup(p.Currency); !ok {
				return c, fmt.Errorf("%s: the currency %q is not one of the supported ones", p.Key, p.Currency)
			}
			if p.Currency == c.Currency {
				p.Currency = ""
			} else if strings.TrimSpace(p.UserPrice) == "" || strings.TrimSpace(p.GBPrice) == "" {
				return c, fmt.Errorf("%s: a customer in another currency (%s) needs both prices in that currency; the default prices are in %s", p.Key, p.Currency, c.Currency)
			}
		}
		for _, f := range []*string{&p.UserPrice, &p.GBPrice} {
			if strings.TrimSpace(*f) == "" {
				*f = ""
				continue
			}
			n, err := ParseDec(*f)
			if err != nil {
				return c, fmt.Errorf("%s: %w", p.Key, err)
			}
			*f = n.String()
		}
		if p.Discount, err = pct(p.Discount, p.Key+": the discount"); err != nil {
			return c, err
		}
		if strings.TrimSpace(p.VAT) != "" {
			if p.VAT, err = pct(p.VAT, p.Key+": the VAT"); err != nil {
				return c, err
			}
		} else {
			p.VAT = ""
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	c.Profiles = out
	return c, nil
}

func pct(s, what string) (string, error) {
	n, err := ParseDec(s)
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	if n > 100*decScale {
		return "", fmt.Errorf("%s must be between 0 and 100 percent", what)
	}
	return n.String(), nil
}

// Profile returns the settings of one instance (an empty one if it has none).
func (c Config) Profile(key string) Profile {
	for _, p := range c.Profiles {
		if p.Key == key {
			return p
		}
	}
	return Profile{Key: key}
}

type stored struct {
	Tenant  string    `json:"tenant"`
	Updated time.Time `json:"updated"`
	Config
}

// Service keeps each tenant's configuration.
type Service struct{ DB docstore.Backend }

func New(db docstore.Backend) *Service { return &Service{DB: db} }

func ctx8() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 8*time.Second)
}

// Get returns the tenant's configuration, or the default (nothing charged) if there is none.
func (s *Service) Get(tenant string) Config {
	c, cancel := ctx8()
	defer cancel()
	doc, err := s.DB.Get(c, coll, tenant)
	if err != nil {
		return Default()
	}
	var st stored
	if doc.Decode(&st) != nil || st.Tenant != tenant {
		return Default()
	}
	if cl, err := st.Config.Clean(); err == nil {
		return cl
	}
	return st.Config
}

// Put validates and saves.
func (s *Service) Put(tenant string, c Config) (Config, error) {
	c, err := c.Clean()
	if err != nil {
		return c, err
	}
	cx, cancel := ctx8()
	defer cancel()
	if err := s.DB.Put(cx, coll, tenant, stored{Tenant: tenant, Updated: time.Now().UTC(), Config: c}, ""); err != nil {
		return c, err
	}
	return c, nil
}
