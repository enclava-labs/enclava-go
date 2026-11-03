// dashboard_format_test.go — relTime/absTime/money/count behavior per the
// 07-02 contract (relative text + absolute title, currency exactly once,
// thousands separators).
package httpx

import (
	"strings"
	"testing"
	"time"
)

func TestRelTime(t *testing.T) {
	now := time.Now()

	got := string(relTime(now.Add(-12 * time.Minute)))
	if !strings.Contains(got, "12 min ago") {
		t.Fatalf("relTime(12min) = %q, want \"12 min ago\"", got)
	}
	if !strings.Contains(got, `<time datetime="`) || !strings.Contains(got, `title="`) {
		t.Fatalf("relTime must emit <time datetime=… title=…>, got %s", got)
	}

	if got := string(relTime(now.Add(-2 * time.Hour))); !strings.Contains(got, "2 h ago") {
		t.Fatalf("relTime(2h) = %q, want \"2 h ago\"", got)
	}
	if got := string(relTime(now)); !strings.Contains(got, "just now") {
		t.Fatalf("relTime(now) = %q, want \"just now\"", got)
	}

	// AUTH-I03: future instants render "in N units", never a past-only
	// phrase that made a session's idle expiry read "just now".
	if got := string(relTime(now.Add(30 * time.Minute))); !strings.Contains(got, "in 30 minutes") {
		t.Fatalf("relTime(+30m) = %q, want \"in 30 minutes\"", got)
	}
	if got := string(relTime(now.Add(2 * time.Hour))); !strings.Contains(got, "in 2 hours") {
		t.Fatalf("relTime(+2h) = %q, want \"in 2 hours\"", got)
	}
	if got := string(relTime(now.Add(30 * time.Second))); !strings.Contains(got, "just now") {
		t.Fatalf("relTime(+30s) = %q, want \"just now\" within a minute either way", got)
	}
	if got := string(relTime(now.Add(3 * 24 * time.Hour))); !strings.Contains(got, "in 3 days") {
		t.Fatalf("relTime(+3d) = %q, want \"in 3 days\"", got)
	}

	// RFC3339 string input, *time.Time input, nil.
	if got := string(relTime(now.Add(-30 * time.Second).UTC().Format(time.RFC3339))); !strings.Contains(got, "just now") {
		t.Fatalf("relTime(RFC3339 string) = %q", got)
	}
	pt := now.Add(-3 * time.Hour)
	if got := string(relTime(&pt)); !strings.Contains(got, "3 h ago") {
		t.Fatalf("relTime(*time.Time) = %q", got)
	}
	if got := string(relTime(nil)); got != "—" {
		t.Fatalf("relTime(nil) = %q, want em dash", got)
	}

	// The title must carry the absolute timestamp.
	tt := now.Add(-12 * time.Minute)
	wantTitle := `title="` + absTime(tt) + `"`
	if !strings.Contains(string(relTime(tt)), wantTitle) {
		t.Fatalf("relTime title must be the absolute time %q", wantTitle)
	}
}

func TestAbsTime(t *testing.T) {
	tt := time.Date(2026, 3, 7, 15, 4, 0, 0, time.UTC)
	if got := absTime(tt); got != "7 Mar 2026, 15:04" {
		t.Fatalf("absTime = %q, want %q", got, "7 Mar 2026, 15:04")
	}
	if got := absTime(nil); got != "" {
		t.Fatalf("absTime(nil) = %q, want empty", got)
	}
	var pt *time.Time
	if got := absTime(pt); got != "" {
		t.Fatalf("absTime(nil pointer) = %q, want empty", got)
	}
	if got := absTime("not-a-time"); got != "" {
		t.Fatalf("absTime(unparseable) = %q, want empty", got)
	}
	if got := absTime("2026-03-07T15:04:00Z"); got != "7 Mar 2026, 15:04" {
		t.Fatalf("absTime(RFC3339 string) = %q", got)
	}
}

func TestMoney(t *testing.T) {
	cases := []struct {
		micros   int64
		currency string
		want     string
	}{
		{1240500000, "USD", "$1,240.50"},
		{-500, "USD", "-$0.00"}, // negative sub-cent keeps the sign shape
		{1240500000, "", "1,240.50"},
		{1240500000, "NOK", "1,240.50 NOK"}, // unknown currency: code once, suffix
		{1240500000, "EUR", "€1,240.50"},
		{1240500000, "GBP", "£1,240.50"},
		{0, "USD", "$0.00"},
		{5000, "USD", "$0.01"}, // rounding boundary: half-cent rounds up
		{4999, "USD", "$0.00"}, // below half-cent rounds down
		{999999999, "USD", "$1,000.00"},
		{-1240500000, "USD", "-$1,240.50"},
	}
	for _, c := range cases {
		if got := money(c.micros, c.currency); got != c.want {
			t.Errorf("money(%d, %q) = %q, want %q", c.micros, c.currency, got, c.want)
		}
	}
	// Anti-dup (source §10): the currency code may appear at most once.
	for _, cur := range []string{"USD", "EUR", "NOK"} {
		if got := money(1240500000, cur); strings.Count(got, cur) > 1 {
			t.Errorf("currency %q appears more than once in %q", cur, got)
		}
	}
}

func TestSpendMoney(t *testing.T) {
	cases := []struct {
		micros   int64
		currency string
		want     string
	}{
		{0, "USD", "$0.00"},
		{1, "USD", "<$0.01"},
		{4999, "EUR", "<€0.01"},
		{5000, "USD", "$0.01"},
		{1, "NOK", "<0.01 NOK"},
		{1, "", "<0.01"},
		{-1, "USD", ">-$0.01"},
		{-5000, "USD", "-$0.01"},
	}
	for _, c := range cases {
		if got := spendMoney(c.micros, c.currency); got != c.want {
			t.Errorf("spendMoney(%d, %q) = %q, want %q", c.micros, c.currency, got, c.want)
		}
	}
	if got := spendMicrosTitle(1, "USD"); got != "1 micros USD" {
		t.Errorf("spendMicrosTitle = %q", got)
	}
}

// TestTokenPrice: rates render per 1M tokens without rounding sub-cent prices.
func TestTokenPrice(t *testing.T) {
	cases := []struct {
		rate, qty int64
		currency  string
		want      string
	}{
		{1000, 1000000, "USD", "$0.001"},   // the $0.001 / 1M case
		{2500000, 1000000, "USD", "$2.50"}, // whole cents still show two places
		{5, 10000000, "USD", "$0.0000005"}, // finer than a micro per 1M tokens
		{1, 1, "EUR", "€1.00"},             // one micro per token
		{1500000000, 1000000, "NOK", "1,500.00 NOK"},
		{0, 1000000, "USD", "$0.00"},
	}
	for _, c := range cases {
		if got := tokenPrice(c.rate, c.qty, c.currency); got != c.want {
			t.Errorf("tokenPrice(%d, %d, %q) = %q, want %q", c.rate, c.qty, c.currency, got, c.want)
		}
	}
}

func TestCount(t *testing.T) {
	cases := []struct {
		v    any
		want string
	}{
		{1234567, "1,234,567"},
		{int64(987654321), "987,654,321"},
		{0, "0"},
		{nil, "0"},
		{1234.5, "1,234.5"},
		{int32(1000), "1,000"},
		{int(-1234567), "-1,234,567"},
		{float64(1234567.89), "1,234,567.89"},
	}
	for _, c := range cases {
		if got := count(c.v); got != c.want {
			t.Errorf("count(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}
