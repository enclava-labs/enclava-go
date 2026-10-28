// dashboard_templates_test.go — unit tests for the 07-02 template machinery
// (D-22), the inline-SVG icon set, the server-rendered charts and the safe
// snippet highlighter.
package httpx

import (
	"bytes"
	"strings"
	"testing"
)

// dashboardFrozenIconNames is the verbatim data.js name list from 07-02-PLAN.
var dashboardFrozenIconNames = []string{
	"home", "chat", "search", "extract", "file", "assistant", "knowledge",
	"sources", "tools", "models", "key", "users", "usage", "billing", "audit",
	"settings", "layers", "shield", "lock", "activity", "bell", "plus",
	"chevronDown", "chevronRight", "chevronLeft", "check", "x", "alert",
	"info", "refresh", "external", "filter", "upload", "send", "stop",
	"clock", "sparkles", "sun", "moon", "menu", "command", "link", "copy",
	"logout", "more",
}

func TestDashboardTemplateSetMissingPage(t *testing.T) {
	_, err := dashboardTemplateSet("nonexistent")
	if err == nil {
		t.Fatal("missing page must return an error")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Fatalf("template error must name its page (D-22), got: %v", err)
	}
}

func TestDashboardTemplateComponentsParse(t *testing.T) {
	base, err := dashboardBaseTemplates()
	if err != nil {
		t.Fatalf("base set (layout+components+partials) parse: %v", err)
	}
	for _, name := range []string{"empty", "page-head", "tabs", "confirm-btn", "drawer-shell"} {
		if base.Lookup(name) == nil {
			t.Errorf("components.tmpl must define %q", name)
		}
	}

	var buf bytes.Buffer
	empty := dashboardEmpty{Icon: "search", Text: "Nothing here", ActionLabel: "Add source", ActionHref: "/dashboard/sources?new=1"}
	if err := base.ExecuteTemplate(&buf, "empty", empty); err != nil {
		t.Fatalf("empty component did not render: %v", err)
	}
	for _, want := range []string{`class="empty"`, "Nothing here", "Add source", `href="/dashboard/sources?new=1"`, "<svg"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("empty render missing %q; got %s", want, buf.String())
		}
	}

	buf.Reset()
	tabs := []dashboardTab{
		{Label: "People", Href: "/dashboard/access/people"},
		{Label: "Roles", Href: "/dashboard/access/roles", Active: true},
	}
	if err := base.ExecuteTemplate(&buf, "tabs", tabs); err != nil {
		t.Fatalf("tabs component did not render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `aria-current="page"`) || strings.Count(out, `aria-current="page"`) != 1 {
		t.Errorf("active tab must carry aria-current exactly once; got %s", out)
	}
	for _, want := range []string{`class="tabs"`, "People", "/dashboard/access/roles", `class="tab active"`} {
		if !strings.Contains(out, want) {
			t.Errorf("tabs render missing %q; got %s", want, out)
		}
	}
}

func TestDashboardTemplateFuncMapFrozenNames(t *testing.T) {
	for _, name := range []string{
		"statusPill", "statusLabel", "statusTone", "term", "relTime",
		"absTime", "money", "count", "icon", "spark", "bars", "mark",
	} {
		if _, ok := dashboardBaseFuncs[name]; !ok {
			t.Errorf("FuncMap missing frozen helper %q", name)
		}
	}
}

func TestDashboardAuthTemplates(t *testing.T) {
	set := dashboardAuthTemplates()
	if set == nil {
		t.Fatal("dashboardAuthTemplates returned nil")
	}
	if !dashboardAuthLoaded {
		t.Skip("auth templates land in 07-05; nothing to execute yet")
	}
	name := "error-403"
	if set.Lookup(name) == nil {
		name = "error-403.tmpl"
	}
	if set.Lookup(name) == nil {
		t.Fatalf("auth set parsed but defines no error-403 template; have: %v", set.Templates())
	}
	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, name, nil); err != nil {
		t.Fatalf("error-403 did not execute: %v", err)
	}
	if DashboardAuthTemplates() != set {
		t.Error("DashboardAuthTemplates must return the same parsed set")
	}
}

func TestIcon(t *testing.T) {
	svg := string(icon("home"))
	if !strings.Contains(svg, `stroke="currentColor"`) || !strings.Contains(svg, `aria-hidden="true"`) {
		t.Fatalf("icon must carry currentColor stroke + aria-hidden, got %s", svg)
	}
	if !strings.Contains(svg, `<path d="M3 10.5 12 3l9 7.5"/>`) {
		t.Fatalf("home icon path not ported from data.js, got %s", svg)
	}
	if string(icon("bogus")) != string(icon("alert")) {
		t.Fatal("unknown icon name must render the loud alert fallback")
	}
	for _, name := range dashboardFrozenIconNames {
		body, ok := dashboardIconPaths[name]
		if !ok || body == "" {
			t.Errorf("frozen icon %q missing from dashboardIconPaths", name)
			continue
		}
		if out := string(icon(name)); !strings.HasPrefix(out, "<svg ") || !strings.HasSuffix(out, "</svg>") {
			t.Errorf("icon %q renders an invalid svg element", name)
		}
	}
	if len(dashboardIconPaths) != len(dashboardFrozenIconNames) {
		t.Errorf("icon set has %d entries, expected %d (verbatim data.js port)", len(dashboardIconPaths), len(dashboardFrozenIconNames))
	}
}

func TestMark(t *testing.T) {
	// "lt" must not match inside the escaped &lt; entity (P7-11).
	if got := string(mark("a < b and wind", "lt")); strings.Contains(got, "<mark>") || !strings.Contains(got, "&lt;") {
		t.Fatalf(`mark("a < b and wind", "lt") = %q, want escaped text with no <mark>`, got)
	}
	if got := string(mark("Northwind", "wind")); got != "North<mark>wind</mark>" {
		t.Fatalf(`mark("Northwind", "wind") = %q`, got)
	}
	if got := string(mark("NorthWind", "wind")); got != "North<mark>Wind</mark>" {
		t.Fatalf("match must be case-insensitive on the original text, got %q", got)
	}
	if got := string(mark("one two one", "one")); got != "<mark>one</mark> two <mark>one</mark>" {
		t.Fatalf("all matches must be wrapped, got %q", got)
	}
	if got := string(mark("abc", "")); got != "abc" {
		t.Fatalf("empty query must escape only, got %q", got)
	}
	// Adversarial: never emits unescaped text (T-07-02-01).
	got := string(mark("<script>alert('x')</script>", "script"))
	if strings.Contains(got, "<script>") || strings.Contains(got, "alert('x')") {
		t.Fatalf("mark must never emit raw markup, got %q", got)
	}
	if !strings.Contains(got, "<mark>script</mark>") || !strings.Contains(got, "&lt;") {
		t.Fatalf("escaped segment must still be <mark>-wrapped, got %q", got)
	}
}

func TestSpark(t *testing.T) {
	got := string(spark([]float64{1, 2, 3}))
	if !strings.Contains(got, "<svg") || !strings.Contains(got, `class="spark"`) {
		t.Fatalf("spark must render an svg.spark, got %s", got)
	}
	if !strings.Contains(got, "<polyline") || strings.Count(got, ",") != 3 {
		t.Fatalf("spark must plot one point per series value, got %s", got)
	}
	if empty := string(spark(nil)); !strings.Contains(empty, `class="spark"`) || strings.Contains(empty, "<polyline") {
		t.Fatalf("empty series must render the placeholder frame, got %s", empty)
	}
}

func TestBars(t *testing.T) {
	got := string(bars([]dashboardBar{
		{Label: "Mon", Value: 10},
		{Label: "Tue", Value: 30},
		{Label: "Wed", Value: 20},
	}))
	if !strings.Contains(got, `<svg class="bar"`) {
		t.Fatalf("bars must render svg.bar (P7-12), got %s", got)
	}
	if n := strings.Count(got, "<rect"); n != 3 {
		t.Fatalf("bars must render one <rect> per value, got %d in %s", n, got)
	}
	for _, label := range []string{"<title>Mon</title>", "<title>Tue</title>", "<title>Wed</title>"} {
		if !strings.Contains(got, label) {
			t.Errorf("each rect must carry its label in <title>; missing %s in %s", label, got)
		}
	}
	// Tue is the max → scaled to full height 40.
	if !strings.Contains(got, `height="40.00"`) {
		t.Errorf("tallest bar must scale to the viewBox height, got %s", got)
	}
	if strings.Contains(got, "style=") {
		t.Errorf("bars must not inline styles — CSS owns color, got %s", got)
	}
	if empty := string(bars(nil)); !strings.Contains(empty, `class="bar"`) || strings.Contains(empty, "<rect") {
		t.Fatalf("empty histogram must render the placeholder frame, got %s", empty)
	}
}
