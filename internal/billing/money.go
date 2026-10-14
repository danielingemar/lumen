// Package billing turns what the Nextcloud instances use (users and stored data) into a basis for invoices: prices that
// the operator chooses, in the currency the operator chooses, per customer if wanted.
//
// Money is never a float: prices are decimals with up to four digits after the point (Dec), amounts are whole minor units
// (öre, cents), and a line is rounded once, half up, the way an invoice shows it.
package billing

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// Currency is one that can be invoiced in. Decimals is how many digits its smallest unit has (2 for krona and euro, 0 for yen).
type Currency struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Symbol   string `json:"symbol"`
	Decimals int    `json:"decimals"`
}

// Currencies are the common ones, most used first. There is no conversion between them: every price is in the currency
// it is set in, and a report adds up each currency by itself.
var Currencies = []Currency{
	{"SEK", "Swedish krona", "kr", 2}, {"EUR", "Euro", "€", 2}, {"USD", "US dollar", "$", 2}, {"GBP", "Pound sterling", "£", 2},
	{"NOK", "Norwegian krone", "kr", 2}, {"DKK", "Danish krone", "kr", 2}, {"CHF", "Swiss franc", "CHF", 2},
	{"ISK", "Icelandic króna", "kr", 0}, {"PLN", "Polish złoty", "zł", 2}, {"CZK", "Czech koruna", "Kč", 2},
	{"CAD", "Canadian dollar", "$", 2}, {"AUD", "Australian dollar", "$", 2}, {"NZD", "New Zealand dollar", "$", 2},
	{"JPY", "Japanese yen", "¥", 0}, {"CNY", "Chinese yuan", "¥", 2}, {"INR", "Indian rupee", "₹", 2},
}

// Lookup finds a currency by its code.
func Lookup(code string) (Currency, bool) {
	for _, c := range Currencies {
		if c.Code == code {
			return c, true
		}
	}
	return Currency{}, false
}

// Dec is a decimal number with four digits after the point, stored as a whole number: 49.5 is 495000.
type Dec int64

const decScale = 10000

var decRe = regexp.MustCompile(`^\d{1,9}(\.\d{1,4})?$`)

// ParseDec reads "49", "49.5" or "49,50" (a comma is accepted as the decimal point). Negative numbers, more than four
// decimals, and numbers above 999 999 999 are refused: they are typing mistakes, not prices.
func ParseDec(s string) (Dec, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", ".")
	if s == "" {
		return 0, nil
	}
	if !decRe.MatchString(s) {
		return 0, fmt.Errorf("%q is not a number (use digits and a decimal point, at most four decimals, not negative)", s)
	}
	whole, frac, _ := strings.Cut(s, ".")
	for len(frac) < 4 {
		frac += "0"
	}
	var w, f int64
	fmt.Sscan(whole, &w)
	for _, ch := range frac {
		f = f*10 + int64(ch-'0')
	}
	return Dec(w*decScale + f), nil
}

// String prints the number with at least two decimals and no trailing zeros beyond that: 49.5 -> "49.50", 0.125 -> "0.125".
func (d Dec) String() string {
	s := fmt.Sprintf("%d.%04d", int64(d)/decScale, int64(d)%decScale)
	for strings.HasSuffix(s, "0") && !strings.HasSuffix(s, ".00") && len(s)-strings.IndexByte(s, '.') > 3 {
		s = s[:len(s)-1]
	}
	return s
}

// Minor prints an amount of minor units as a plain decimal ("12345" with 2 decimals is "123.45"), for files.
func Minor(a int64, decimals int) string {
	neg := a < 0
	if neg {
		a = -a
	}
	s := fmt.Sprint(a)
	if decimals > 0 {
		for len(s) <= decimals {
			s = "0" + s
		}
		s = s[:len(s)-decimals] + "." + s[len(s)-decimals:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

// amount is qty × price in minor units, rounded half up once: qty is in thousandths, price a Dec, and the result is
// multiplied by num/den (the share of the month, 1/1 when everything is charged).
func amount(qtyMilli int64, price Dec, decimals int, num, den int64) int64 {
	if qtyMilli <= 0 || price <= 0 || den <= 0 || num <= 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(qtyMilli), big.NewInt(int64(price)))
	n.Mul(n, big.NewInt(num))
	n.Mul(n, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
	d := new(big.Int).Mul(big.NewInt(1000*decScale), big.NewInt(den))
	return roundDiv(n, d)
}

// percentOf is pct percent of an amount of minor units, rounded half up.
func percentOf(minor int64, pct Dec) int64 {
	if minor <= 0 || pct <= 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(minor), big.NewInt(int64(pct)))
	return roundDiv(n, big.NewInt(100*decScale))
}

func roundDiv(n, d *big.Int) int64 {
	two := big.NewInt(2)
	n = new(big.Int).Add(new(big.Int).Mul(n, two), d)
	n.Div(n, new(big.Int).Mul(d, two))
	return n.Int64()
}
