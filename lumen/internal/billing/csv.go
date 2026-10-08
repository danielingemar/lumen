package billing

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
)

// WriteCSV writes the report as a file for a spreadsheet or an invoicing program. view is "lines" (one row per instance) or
// "invoices" (one row per customer and currency). With locale "sv" the file is made for Swedish spreadsheet programs:
// semicolons between columns, a decimal comma, and a byte order mark so that å, ä and ö survive.
func WriteCSV(w io.Writer, r Report, view, locale string) error {
	sv := locale == "sv"
	if sv {
		if _, err := io.WriteString(w, "\xef\xbb\xbf"); err != nil {
			return err
		}
	}
	cw := csv.NewWriter(w)
	if sv {
		cw.Comma = ';'
	}
	num := func(s string) string {
		if sv {
			return strings.ReplaceAll(s, ".", ",")
		}
		return s
	}
	money := func(a int64, dec int) string { return num(Minor(a, dec)) }
	clean := func(s string) string { return csvSafe(s) }
	if view == "invoices" {
		cw.Write([]string{"period", "customer", "currency", "instances", "net", "vat", "total"})
		for _, in := range r.Invoices {
			cw.Write([]string{r.Period, clean(in.Customer), in.Currency, fmt.Sprint(len(in.Lines)), money(in.Net, in.Decimals), money(in.VAT, in.Decimals), money(in.Total, in.Decimals)})
		}
		cw.Flush()
		return cw.Error()
	}
	cw.Write([]string{"period", "customer", "instance", "currency", "users", "user_price", "users_amount", "storage_" + r.GBUnit, "gb_price", "storage_amount",
		"discount_percent", "discount", "net", "vat_percent", "days_reported", "note"})
	for _, in := range r.Invoices {
		for _, l := range in.Lines {
			cw.Write([]string{r.Period, clean(in.Customer), clean(l.Name), in.Currency, num(trim3(l.Users)), num(l.UserPrice), money(l.UserAmount, in.Decimals),
				num(trim3(l.GB)), num(l.GBPrice), money(l.DataAmount, in.Decimals), num(l.DiscountPct), money(l.Discount, in.Decimals), money(l.Net, in.Decimals),
				num(l.VATPercent), fmt.Sprint(l.DaysReported), clean(l.Note)})
		}
	}
	cw.Flush()
	return cw.Error()
}

func trim3(f float64) string {
	s := fmt.Sprintf("%.3f", f)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// csvSafe stops a customer or note from being read as a formula by a spreadsheet program (a name that starts with = + - @).
func csvSafe(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 {
			return ' '
		}
		return r
	}, s)
	if s != "" && strings.ContainsRune("=+-@", rune(s[0])) {
		return "'" + s
	}
	return s
}
