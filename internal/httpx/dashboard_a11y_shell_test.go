// dashboard_a11y_shell_test.go — OA-38 rendered a11y semantics for the shared
// chrome: every assertion runs against the REAL layout/components executed by
// renderDashboardInto with a probe content template (the same path
// dashboardTemplateSet takes), never a template-source grep. Assertions cover
// the landmark/focus-order contract, accessible names on icon-only controls
// (visible text counts as a name), aria-current wiring, the drawer/toast
// shell semantics and group headings rendered only with visible items.
// DOM helpers shared with dashboard_a11y_pages_test.go (hasClass/attr/
// textOf/accessibleName/adminCaps/memberCaps).
package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/auth"

	"golang.org/x/net/html"
)

func attrVal(n *html.Node, key string) string {
	v, _ := attr(n, key)
	return v
}

// renderA11yPage renders the production layout + components around a probe
// content template through the real renderer and returns the parsed document.
// caps drives the role branch (nav membership, lockhints, conf endpoints).
func renderA11yPage(t *testing.T, caps dashboardCaps) *html.Node {
	t.Helper()
	base, err := dashboardBaseTemplates()
	if err != nil {
		t.Fatalf("base set with layout: %v", err)
	}
	set, err := base.Clone()
	if err != nil {
		t.Fatal(err)
	}
	// The shell contract: exactly one h1 per page, inside .headcopy.
	if _, err := set.Parse(`{{define "content"}}<header class="head"><div class="headcopy"><h1>Probe</h1></div></header>{{end}}`); err != nil {
		t.Fatal(err)
	}

	app := &App{}
	req := httptest.NewRequest("GET", "/dashboard/chat", nil)
	ctx := auth.WithPrincipal(req.Context(), auth.Principal{TenantID: "tenant-1", UserID: "user-1"})
	ctx = withCaps(ctx, caps)
	rec := httptest.NewRecorder()
	app.renderDashboardInto(rec, req.WithContext(ctx), set, "page", "chat", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("shell render status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	doc, err := html.Parse(strings.NewReader(rec.Body.String()))
	if err != nil {
		t.Fatalf("rendered shell did not parse: %v", err)
	}
	return doc
}

func walkAll(n *html.Node, match func(*html.Node) bool, out *[]*html.Node) {
	if match(n) {
		*out = append(*out, n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkAll(c, match, out)
	}
}

func elementByID(n *html.Node, id string) *html.Node {
	var hit *html.Node
	var found bool
	var rec func(*html.Node)
	rec = func(m *html.Node) {
		if found {
			return
		}
		if m.Type == html.ElementNode && attrVal(m, "id") == id {
			hit, found = m, true
			return
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return hit
}

func isFocusable(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == "template" {
			return false // template content is inert — not in tab order
		}
	}
	switch n.Data {
	case "a":
		_, ok := attr(n, "href")
		return ok
	case "button":
		_, disabled := attr(n, "disabled")
		return !disabled
	case "input":
		_, disabled := attr(n, "disabled")
		return attrVal(n, "type") != "hidden" && !disabled
	case "select", "textarea":
		_, disabled := attr(n, "disabled")
		return !disabled
	}
	if t, ok := attr(n, "tabindex"); ok && t != "-1" {
		return true
	}
	return false
}

// TestA11yShellLandmarks asserts the focus-order/landmark contract on the
// rendered shell: skip link first, #content inside <main>, exactly one h1 in
// .headcopy, aria-current on exactly the active navlink, the closed drawer's
// absent dialog semantics, the single polite live region and the labeled
func TestA11yShellLandmarks(t *testing.T) {
	doc := renderA11yPage(t, adminCaps)

	// First focusable element in DOM order is the skip link to #content.
	var focusables []*html.Node
	walkAll(doc, isFocusable, &focusables)
	if len(focusables) == 0 {
		t.Fatal("rendered shell has no focusable elements")
	}
	first := focusables[0]
	if attrVal(first, "href") != "#content" || !hasClass(first, "skip") {
		t.Fatalf("first focusable element must be the .skip link to #content, got <%s class=%q>", first.Data, attrVal(first, "class"))
	}

	// #content lives inside the <main> landmark and is programmatically
	// focusable so the skip link actually moves focus, not just scrolls.
	content := elementByID(doc, "content")
	if content == nil {
		t.Fatal("rendered shell has no #content target")
	}
	if p := content.Parent; p == nil || p.Data != "main" {
		t.Fatal("#content is not inside the <main> landmark")
	}
	if attrVal(content, "tabindex") != "-1" {
		t.Fatal("#content must carry tabindex=-1 so skip-link activation moves focus")
	}

	// Exactly one h1 in the page, inside .headcopy.
	var h1s []*html.Node
	walkAll(doc, func(n *html.Node) bool { return n.Type == html.ElementNode && n.Data == "h1" }, &h1s)
	if len(h1s) != 1 {
		t.Fatalf("rendered page must carry exactly one h1, got %d", len(h1s))
	}
	if p := h1s[0].Parent; p == nil || !hasClass(p, "headcopy") {
		t.Fatal("the single h1 must live inside .headcopy")
	}

	// Exactly one active navlink carries aria-current="page" (chat fixture).
	var currents []*html.Node
	walkAll(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && attrVal(n, "aria-current") == "page"
	}, &currents)
	if len(currents) != 1 || !hasClass(currents[0], "navlink") {
		t.Fatalf("exactly one navlink must carry aria-current=page, got %d", len(currents))
	}
	if attrVal(currents[0], "href") != "/dashboard/chat" {
		t.Fatalf("aria-current landed on %q, want the active chat navlink", attrVal(currents[0], "href"))
	}

	// The shared drawer is a plain hidden aside while closed — app.js stamps
	// role=dialog/aria-modal/aria-labelledby on it only for the duration of an
	// open (UX-H05: a closed off-canvas aside is not a dialog landmark).
	drawer := elementByID(doc, "drawer")
	if drawer == nil || attrVal(drawer, "role") != "" || attrVal(drawer, "aria-modal") != "" {
		t.Fatal("closed #drawer must render without dialog semantics (app.js assigns them on open)")
	}
	if _, hidden := attr(drawer, "hidden"); !hidden {
		t.Fatal("closed #drawer must render hidden")
	}
	toast := elementByID(doc, "toast")
	if toast == nil || attrVal(toast, "aria-live") != "polite" {
		t.Fatal("#toast must render a single aria-live=polite region")
	}
	if elementByID(doc, "backdrop") == nil || elementByID(doc, "pop") == nil {
		t.Fatal("drawer shell must keep #backdrop and #pop hooks")
	}

	// The mobile status dot mirrors the confidential card's tone via title.
	var status []*html.Node
	walkAll(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "mobile-status")
	}, &status)
	if len(status) != 1 || strings.TrimSpace(attrVal(status[0], "title")) == "" {
		t.Fatal(".mobile-status must carry a nonempty title")
	}

	// Group headings render only inside groups that have visible items: every
	// rendered .group-title's group holds at least one navlink.
	var titles []*html.Node
	walkAll(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "group-title")
	}, &titles)
	if len(titles) != 3 {
		t.Fatalf("admin render should show 3 nav groups, got %d", len(titles))
	}
	for _, title := range titles {
		var links []*html.Node
		walkAll(title.Parent, func(n *html.Node) bool {
			return n.Type == html.ElementNode && hasClass(n, "navlink")
		}, &links)
		if len(links) == 0 {
			t.Fatalf("group title %q rendered with zero navlinks", textOf(title))
		}
	}
}

// TestIconNames: every icon-only or ambiguous chrome control rendered by the
// layout carries a nonempty accessible name — .iconbtn controls, the org
// button, the confidential card, the user-menu toggle — and the user-menu
// template items are real buttons/links, not generic containers.
func TestIconNames(t *testing.T) {
	doc := renderA11yPage(t, adminCaps)
	empty := map[string]string{}

	var iconbtns []*html.Node
	walkAll(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "iconbtn")
	}, &iconbtns)
	if len(iconbtns) < 2 {
		t.Fatalf("shell renders fewer iconbtn controls than the chrome requires: %d", len(iconbtns))
	}
	for _, b := range iconbtns {
		if accessibleName(b, empty, empty) == "" {
			t.Fatalf("iconbtn %s renders without an accessible name", attrVal(b, "class"))
		}
	}

	for _, cls := range []string{"org", "conf-card", "user-menu"} {
		var nodes []*html.Node
		walkAll(doc, func(n *html.Node) bool {
			return n.Type == html.ElementNode && hasClass(n, cls)
		}, &nodes)
		if len(nodes) != 1 {
			t.Fatalf(".%s rendered %d times, want 1", cls, len(nodes))
		}
		if nodes[0].Data != "button" && nodes[0].Data != "a" {
			t.Fatalf(".%s must be a real button/link, is <%s>", cls, nodes[0].Data)
		}
		if accessibleName(nodes[0], empty, empty) == "" {
			t.Fatalf(".%s carries no accessible name", cls)
		}
	}

	// The user-menu popover template renders only real buttons/links — the
	// template's content fragment, not its (empty) tree placeholder.
	tpl := elementByID(doc, "user-menu-tpl")
	if tpl == nil {
		t.Fatal("user-menu-tpl missing")
	}
	var items []*html.Node
	walkAll(tpl, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "navlink")
	}, &items)
	if len(items) != 3 {
		t.Fatalf("user menu must offer Account/Theme/Sign out, found %d items", len(items))
	}
	for _, it := range items {
		if it.Data != "a" && it.Data != "button" {
			t.Fatalf("user-menu item is <%s>, want a link or button", it.Data)
		}
		if accessibleName(it, empty, empty) == "" {
			t.Fatal("user-menu item without an accessible name")
		}
	}
}

// TestSkipLink verifies the rendered focus-order contract end to end: the
// skip link precedes every other focusable element and points at an
// existing, programmatically focusable landmark child.
func TestSkipLink(t *testing.T) {
	doc := renderA11yPage(t, adminCaps)

	var links []*html.Node
	walkAll(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "skip")
	}, &links)
	if len(links) != 1 {
		t.Fatalf("expected exactly one skip link, got %d", len(links))
	}
	if attrVal(links[0], "href") != "#content" {
		t.Fatalf("skip link href = %q, want #content", attrVal(links[0], "href"))
	}
	if strings.TrimSpace(textOf(links[0])) == "" {
		t.Fatal("skip link has no label")
	}
	if elementByID(doc, "content") == nil {
		t.Fatal("skip link target #content is absent from the rendered page")
	}

	// Nothing focusable may precede it.
	var before int
	var seen bool
	walkAll(doc, func(n *html.Node) bool {
		if n == links[0] {
			seen = true
		}
		if !seen && isFocusable(n) {
			before++
		}
		return false
	}, &[]*html.Node{})
	if before != 0 {
		t.Fatalf("%d focusable element(s) precede the skip link", before)
	}
}

// TestA11yShellMemberRender renders the shell under a member cap set: the
// conf card keeps its button semantics + name with zero endpoints, ADMIN
// lockhints stay off member nav, and no ADMIN group heading renders.
func TestA11yShellMemberRender(t *testing.T) {
	doc := renderA11yPage(t, memberCaps)
	empty := map[string]string{}

	var locks []*html.Node
	walkAll(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "lockhint")
	}, &locks)
	if len(locks) != 0 {
		t.Fatalf("member render leaks %d admin lockhints", len(locks))
	}
	var groups []*html.Node
	walkAll(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "group-title")
	}, &groups)
	for _, g := range groups {
		if strings.Contains(textOf(g), "ADMIN") {
			t.Fatal("ADMIN group heading rendered for a member with no admin items")
		}
	}
	var conf []*html.Node
	walkAll(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "conf-card")
	}, &conf)
	if len(conf) != 1 || conf[0].Data != "button" || accessibleName(conf[0], empty, empty) == "" {
		t.Fatal("conf-card must stay a named button with zero endpoints")
	}
	if attrVal(conf[0], "aria-haspopup") != "dialog" || attrVal(conf[0], "aria-expanded") != "false" {
		t.Fatal("conf-card must wire aria-haspopup=dialog and aria-expanded=false when closed")
	}
	var menus []*html.Node
	walkAll(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "mobile-menu")
	}, &menus)
	if len(menus) != 1 || attrVal(menus[0], "aria-expanded") != "false" || attrVal(menus[0], "aria-controls") != "sidebar" {
		t.Fatal("mobile-menu must render aria-expanded=false aria-controls=sidebar")
	}
}
