// dashboard_charts.go — server-rendered SVG charts (P7-12): real <rect> bars
// with <title> tooltips instead of the mockup's styled <i> elements; CSS owns
// all colors (class only, no inline styling).
package httpx

import (
	"fmt"
	"html/template"
	"strings"
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
type dashboardBar struct {
	Label string
	Value float64
}

// bars renders a real .bar SVG histogram of <rect> columns scaled to the max
// value; each rect carries a <title> with its label (native tooltip/AT name).
func bars(values []dashboardBar) template.HTML {
	const w, h = 100.0, 40.0
	open := fmt.Sprintf(`<svg class="bar" viewBox="0 0 %g %g">`, w, h)
	if len(values) == 0 {
		return template.HTML(open + `</svg>`) //nolint:gosec // SVG consists only of fixed markup and numeric constants.
	}
	max := 0.0
	for _, v := range values {
		if v.Value > max {
			max = v.Value
		}
	}
	var b strings.Builder
	b.WriteString(open)
	slot := w / float64(len(values))
	width := slot * 0.7 // leave a gap between columns
	for i, v := range values {
		height := 0.0
		if max > 0 {
			height = h * v.Value / max
		}
		x := float64(i)*slot + (slot-width)/2
		fmt.Fprintf(&b, `<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f"><title>%s</title></rect>`,
			x, h-height, width, height, template.HTMLEscapeString(v.Label))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String()) //nolint:gosec // Labels are HTML-escaped and geometry is numeric.
}
