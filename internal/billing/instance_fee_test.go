package billing

import (
	"bytes"
	"strings"
	"testing"
)

func TestAFixedFeePerInstanceIsAddedAndACustomerCanHaveItsOwn(t *testing.T) {
	cfg := Config{Currency: "SEK", UserPrice: "50", GBPrice: "2", InstancePrice: "200", VAT: "25", Profiles: []Profile{
		{Key: "special", Customer: "Special", InstancePrice: "150"}, // its own fee
		{Key: "free", Customer: "Free", InstancePrice: "0"},         // no fee at all, although the default is 200
	}}
	m := []Month{
		{Instance: "plain", Month: "2026-10", Days: days(1, 31, 10, 100)},
		{Instance: "special", Month: "2026-10", Days: days(1, 31, 10, 100)},
		{Instance: "free", Month: "2026-10", Days: days(1, 31, 10, 100)},
	}
	r, err := Build(cfg, "2026-10", m, nil, oct)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Line{}
	for _, in := range r.Invoices {
		for _, l := range in.Lines {
			got[l.Key] = l
		}
	}
	// users 10 × 50 = 500.00 and data 100 × 2 = 200.00, plus the fee
	if l := got["plain"]; l.InstAmount != 20000 || l.InstPrice != "200.00" || l.Gross != 90000 {
		t.Fatalf("the default fee: %+v", l)
	}
	if l := got["special"]; l.InstAmount != 15000 || l.Gross != 85000 {
		t.Fatalf("its own fee: %+v", l)
	}
	if l := got["free"]; l.InstAmount != 0 || l.Gross != 70000 {
		t.Fatalf("an own fee of 0 is no fee: %+v", l)
	}
	// the fee is part of what VAT is charged on: 900.00 net, 225.00 VAT
	for _, in := range r.Invoices {
		if in.Customer == "plain" && (in.Net != 90000 || in.VAT != 22500) {
			t.Fatalf("%+v", in)
		}
	}
}

func TestTheFeeFollowsProratingAndIsNotChargedWithoutData(t *testing.T) {
	cfg := Config{InstancePrice: "310", Prorate: true}
	m := []Month{{Instance: "a", Month: "2026-10", Days: days(22, 31, 1, 0)}} // 10 of 31 days
	r, _ := Build(cfg, "2026-10", m, []Known{{"b", "B"}}, oct)                // b is known but reported nothing
	var a, b Line
	for _, in := range r.Invoices {
		for _, l := range in.Lines {
			if l.Key == "a" {
				a = l
			}
			if l.Key == "b" {
				b = l
			}
		}
	}
	if a.InstAmount != 10000 { // 310.00 × 10/31
		t.Fatalf("%+v", a)
	}
	if b.InstAmount != 0 || b.Gross != 0 {
		t.Fatalf("an instance that reported nothing is not charged: %+v", b)
	}
	cfg.Prorate = false
	r, _ = Build(cfg, "2026-10", m, nil, oct)
	if got := r.Invoices[0].Lines[0].InstAmount; got != 31000 {
		t.Fatalf("the whole fee when not prorated: %d", got)
	}
}

func TestACustomerInAnotherCurrencyHasNoFeeUnlessItGetsItsOwn(t *testing.T) {
	cfg := Config{Currency: "SEK", UserPrice: "10", InstancePrice: "200", Profiles: []Profile{
		{Key: "eur1", Customer: "E1", Currency: "EUR", UserPrice: "1", GBPrice: "0.1"},
		{Key: "eur2", Customer: "E2", Currency: "EUR", UserPrice: "1", GBPrice: "0.1", InstancePrice: "15"},
	}}
	m := []Month{{Instance: "eur1", Month: "2026-10", Days: days(1, 31, 1, 0)}, {Instance: "eur2", Month: "2026-10", Days: days(1, 31, 1, 0)}}
	r, _ := Build(cfg, "2026-10", m, nil, oct)
	for _, in := range r.Invoices {
		l := in.Lines[0]
		switch l.Key {
		case "eur1": // 200 kronor must not turn into 200 euro
			if l.InstAmount != 0 {
				t.Fatalf("the default fee is in kronor: %+v", l)
			}
		case "eur2":
			if l.InstAmount != 1500 || in.Currency != "EUR" {
				t.Fatalf("%+v", l)
			}
		}
	}
}

func TestAConfigurationSavedBeforeTheFeeStillWorks(t *testing.T) {
	c, err := Config{Currency: "SEK", UserPrice: "50"}.Clean() // no instance_price in it
	if err != nil || c.InstancePrice != "0.00" {
		t.Fatalf("%v %+v", err, c)
	}
	c, err = Config{Currency: "SEK", InstancePrice: "199,5"}.Clean()
	if err != nil || c.InstancePrice != "199.50" {
		t.Fatalf("%v %+v", err, c)
	}
	for _, bad := range []Config{{InstancePrice: "-1"}, {InstancePrice: "abc"}, {Profiles: []Profile{{Key: "a", InstancePrice: "x"}}}} {
		if _, err := bad.Clean(); err == nil {
			t.Errorf("%+v must be refused", bad)
		}
	}
}

func TestTheFeeIsInTheCSV(t *testing.T) {
	cfg := Config{Currency: "SEK", UserPrice: "10", InstancePrice: "199.5"}
	r, _ := Build(cfg, "2026-10", []Month{{Instance: "a", Month: "2026-10", Days: days(1, 31, 1, 0)}}, nil, oct)
	var b bytes.Buffer
	WriteCSV(&b, r, "lines", "")
	out := b.String()
	if !strings.Contains(out, "instance_price,instance_amount") || !strings.Contains(out, "199.50,199.50") {
		t.Fatalf("%s", out)
	}
}
