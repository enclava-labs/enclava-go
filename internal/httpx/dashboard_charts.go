// dashboard_charts.go — server-rendered SVG charts (P7-12): real <rect> bars
// with <title> tooltips instead of the mockup's styled <i> elements; CSS owns
// all colors (class only, no inline styling).
package httpx

import (
	"fmt"
	"html/template"
	"strings"
	"time"
)

// spark renders the KPI trend as a .spark polyline SVG scaled to the series;
// empty/nil series render an empty placeholder frame.
func spark(series []float64) template.HTML {
	const w, h, pad = 100.0, 24.0, 2.0
	open := fmt.Sprintf(`<svg class="spark" viewBox="0 0 %g %g" aria-hidden="true">`, w, h)
	if len(series) == 0 {
		return template.HTML(open + `</svg>`) //nolint:gosec // SVG consists only of fixed markup and numeric constants.
	}
	min, max := series[0], series[0]
	for _, v := range series {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	span := max - min
	points := make([]string, len(series))
	for i, v := range series {
		x := pad
		if len(series) > 1 {
			x = pad + (w-2*pad)*float64(i)/float64(len(series)-1)
		}
		y := (h-2*pad)/2 + pad // flat series rides the midline
		if span > 0 {
			y = pad + (h-2*pad)*(max-v)/span
		}
		points[i] = fmt.Sprintf("%.2f,%.2f", x, y)
	}
	return template.HTML(open + fmt.Sprintf(`<polyline points="%s"/></svg>`, strings.Join(points, " "))) //nolint:gosec // Points are formatted numeric values.
}

// dashboardBar is one histogram column (frozen input shape for bars()).
// Date is the optional ISO day ("2006-01-02") behind the column; when set it
// drives the axis labels and the "20 Nov · N requests" tooltip.
type dashboardBar struct {
	Label string
	Value float64
	Date  string // ISO day, "" for label-only fallback
}

// bars renders the daily-requests histogram readable for 1–90 points: a 1px
// baseline, columns capped at 28px laid left-to-right in order, a muted
// y-max label top-left, the first/last day under the baseline, the latest
// value above its bar and a per-column title tooltip. Zero days render as a
// 1px stub so gaps stay visible. CSS owns every color; this emits structure
// and geometry only.
func bars(values []dashboardBar) template.HTML {
	if len(values) == 0 {
		return `<div class="bar"></div>`
	}
	max := 0.0
	var total int64
	for _, v := range values {
		if v.Value > max {
			max = v.Value
		}
		total += int64(v.Value)
	}
	day := func(i int) string { // "20 Nov", "" when the column has no usable Date
		t, err := time.Parse("2006-01-02", values[i].Date)
		if err != nil {
			return ""
		}
		return t.Format("2 Jan")
	}
	noun := func(n int64) string {
		if n == 1 {
			return "request"
		}
		return "requests"
	}
	tooltip := func(i int) string {
		if d := day(i); d != "" {
			n := int64(values[i].Value)
			return d + " · " + dashboardCountInt(n) + " " + noun(n)
		}
		return values[i].Label
	}
	aria := "Daily requests · " + dashboardCountInt(total) + " total"
	if first, last := day(0), day(len(values)-1); first != "" && len(values) == 1 {
		aria = "Daily requests on " + first + " · " + dashboardCountInt(total) + " total"
	} else if first != "" {
		aria = "Daily requests from " + first + " to " + last + " · " + dashboardCountInt(total) + " total"
	}
	var b strings.Builder
	b.WriteString(`<div class="barchart" role="img" aria-label="` + template.HTMLEscapeString(aria) + `">`)
	b.WriteString(`<span class="bar-ymax">` + dashboardCountInt(int64(max)) + `</span>`)
	b.WriteString(`<div class="bar">`)
	for i, v := range values {
		cls := "bar-col"
		if v.Value <= 0 {
			cls += " bar-zero"
		}
		if i == len(values)-1 {
			cls += " bar-latest"
		}
		fmt.Fprintf(&b, `<i class="%s" title="%s"`, cls, template.HTMLEscapeString(tooltip(i)))
		if v.Value > 0 && max > 0 {
			fmt.Fprintf(&b, ` style="height:%.2f%%"`, v.Value/max*100)
		}
		b.WriteString(`>`)
		if i == len(values)-1 {
			b.WriteString(`<b>` + dashboardCountInt(int64(v.Value)) + `</b>`)
		}
		if i == 0 && day(0) != "" {
			b.WriteString(`<em>` + template.HTMLEscapeString(day(0)) + `</em>`)
		}
		if i == len(values)-1 && len(values) > 1 && day(i) != "" {
			b.WriteString(`<em>` + template.HTMLEscapeString(day(i)) + `</em>`)
		}
		b.WriteString(`</i>`)
	}
	b.WriteString(`</div></div>`)
	return template.HTML(b.String()) //nolint:gosec // Labels are HTML-escaped and geometry is numeric.
}
