// dashboard_format.go — presentation formatters for the dashboard templates.
// money() renders the currency exactly once (source §10 bug: "USD USD").
package httpx

import (
	"fmt"
	"html/template"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// dashboardAsTime coerces template-passed values: time.Time, *time.Time
// (nil-safe) and RFC3339 strings. nil/zero/unparseable → not ok.
func dashboardAsTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case nil:
		return time.Time{}, false
	case time.Time:
		return t, !t.IsZero()
	case *time.Time:
		if t == nil {
			return time.Time{}, false
		}
		return *t, !t.IsZero()
	case string:
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// absTime renders the absolute timestamp for title= attributes, always UTC:
// "2 Jan 2006, 15:04". Unset values render "".
func absTime(v any) string {
	t, ok := dashboardAsTime(v)
	if !ok {
		return ""
	}
	return t.UTC().Format("2 Jan 2006, 15:04")
}

// relTime renders <time datetime=… title=absTime>relative</time>: within a
// minute either way "just now"; past <60m "N min ago", <48h "N h ago", else
// the absolute date; future instants "in N minute(s)/hour(s)/day(s)" — a
// session's idle expiry is a future instant where a bare past-only render
// would read "just now" (AUTH-I03). nil/unset values render "—".
func relTime(v any) template.HTML {
	t, ok := dashboardAsTime(v)
	if !ok {
		return "—"
	}
	var rel string
	switch d := time.Since(t); {
	case d > -time.Minute && d < time.Minute:
		rel = "just now"
	case d < 0:
		switch a := -d; {
		case a < time.Hour:
			rel = "in " + relUnit(a, time.Minute, "minute")
		case a < 48*time.Hour:
			rel = "in " + relUnit(a, time.Hour, "hour")
		default:
			rel = "in " + relUnit(a, 24*time.Hour, "day")
		}
	default:
		switch {
		case d < time.Hour:
			rel = fmt.Sprintf("%d min ago", int(d.Minutes()))
		case d < 48*time.Hour:
			rel = fmt.Sprintf("%d h ago", int(d.Hours()))
		default:
			rel = t.Format("2 Jan 2006")
		}
	}
	return template.HTML(fmt.Sprintf(`<time datetime="%s" title="%s">%s</time>`,
		t.Format(time.RFC3339),
		template.HTMLEscapeString(absTime(v)),
		template.HTMLEscapeString(rel)))
}

// relUnit renders a duration in whole units with an English plural:
// "1 minute", "30 minutes".
func relUnit(d, unit time.Duration, name string) string {
	n := int64(math.Round(d.Seconds() / unit.Seconds()))
	if n < 1 {
		n = 1
	}
	if n == 1 {
		return fmt.Sprintf("1 %s", name)
	}
	return fmt.Sprintf("%d %ss", n, name)
}

// dashboardMoneySymbols are the currencies shown with a symbol prefix; any
// other currency renders as "1,240.50 CODE" (code once, suffix).
var dashboardMoneySymbols = map[string]string{
	"USD": "$",
	"EUR": "€",
	"GBP": "£",
}

// money formats micros (millionths) with thousands separators and two
// decimals; the currency appears exactly once — symbol prefix for USD/EUR/GBP,
// "amount CODE" suffix otherwise, bare amount when currency is empty.
// Unpriced items are handled by caller copy, not a special value here.
func money(micros int64, currency string) string {
	neg := micros < 0
	if neg {
		micros = -micros
	}
	cents := (micros + 5000) / 10000 // micros → cents, round half up
	amount := dashboardGroupDigits(strconv.FormatInt(cents/100, 10)) +
		fmt.Sprintf(".%02d", cents%100)
	if sym, ok := dashboardMoneySymbols[currency]; ok {
		amount = sym + amount
	} else if currency != "" {
		amount += " " + currency
	}
	if neg {
		amount = "-" + amount
	}
	return amount
}

// spendMoney preserves visibility of nonzero spend below the rounding threshold.
func spendMoney(micros int64, currency string) string {
	if micros > 0 && micros < 5000 {
		return "<" + money(10000, currency)
	}
	if micros < 0 && micros > -5000 {
		return ">" + money(-10000, currency)
	}
	return money(micros, currency)
}

func spendMicrosTitle(micros int64, currency string) string {
	return fmt.Sprintf("%d micros %s", micros, currency)
}

// tokenPrice renders a rate as currency per 1M tokens, the way price lists
// quote it: rate_micros per unit_quantity tokens is exactly
// rate_micros/unit_quantity currency units per million, so sub-cent prices
// such as $0.001 keep their digits where money() would round them to $0.00.
func tokenPrice(rateMicros, unitQuantity int64, currency string) string {
	if unitQuantity <= 0 {
		return ""
	}
	whole, frac, _ := strings.Cut(new(big.Rat).SetFrac64(rateMicros, unitQuantity).FloatString(12), ".")
	frac = strings.TrimRight(frac, "0")
	for len(frac) < 2 {
		frac += "0"
	}
	amount := dashboardGroupDigits(whole) + "." + frac
	if sym, ok := dashboardMoneySymbols[currency]; ok {
		return sym + amount
	}
	if currency != "" {
		return amount + " " + currency
	}
	return amount
}

// count renders a number with thousands separators; nil → "0".
func count(v any) string {
	switch n := v.(type) {
	case nil:
		return "0"
	case int:
		return dashboardCountInt(int64(n))
	case int32:
		return dashboardCountInt(int64(n))
	case int64:
		return dashboardCountInt(n)
	case uint:
		return dashboardGroupDigits(strconv.FormatUint(uint64(n), 10))
	case uint32:
		return dashboardGroupDigits(strconv.FormatUint(uint64(n), 10))
	case uint64:
		return dashboardGroupDigits(strconv.FormatUint(n, 10))
	case float32:
		return dashboardGroupNumber(float64(n))
	case float64:
		return dashboardGroupNumber(n)
	default:
		return fmt.Sprint(v)
	}
}

func dashboardCountInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	if strings.HasPrefix(s, "-") {
		return "-" + dashboardGroupDigits(s[1:])
	}
	return dashboardGroupDigits(s)
}

// dashboardGroupDigits inserts thousands separators into a digit string.
func dashboardGroupDigits(digits string) string {
	n := len(digits)
	if n <= 3 {
		return digits
	}
	var b strings.Builder
	head := n % 3
	if head > 0 {
		b.WriteString(digits[:head])
	}
	for ; head+3 <= n; head += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(digits[head : head+3])
	}
	return b.String()
}

// dashboardGroupNumber groups a formatted float's integer part, keeping any
// fraction: 1234567.5 → "1,234,567.5".
func dashboardGroupNumber(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return sign + dashboardGroupDigits(s[:i]) + s[i:]
	}
	return sign + dashboardGroupDigits(s)
}
