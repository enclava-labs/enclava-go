// dashboard_view_test.go — the shell renderer: the real layout/components
// render a probe page, the buffer rule never commits a partial 200 (P7-10),
// a missing page 500s naming the page (D-22), and the theme cookie maps to
// the body class.
package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/auth"
)

// TestRenderDashboardShell drives renderDashboardInto with the real base set
// (layout + components) plus a parsed probe page — the same path
// dashboardTemplateSet takes, without adding a fixture page to the embed FS.
func TestRenderDashboardShell(t *testing.T) {
	base, err := dashboardBaseTemplates()
	if err != nil {
		t.Fatalf("base set with layout: %v", err)
	}
	set, err := base.Clone()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Parse(`{{define "content"}}<h1>Probe page</h1><p data-org>{{.Org}}</p>{{end}}`); err != nil {
		t.Fatal(err)
	}

	app := &App{}
	req := httptest.NewRequest("GET", "/dashboard/knowledge", nil)
	ctx := auth.WithPrincipal(req.Context(), auth.Principal{TenantID: "tenant-1", UserID: "user-1"})
	// page names BOTH the template file and the nav ID — "knowledge" activates
	// the Knowledge nav item (cap search).
	ctx = withCaps(ctx, dashboardCaps{Chat: true, Search: true})
	rec := httptest.NewRecorder()
	app.renderDashboardInto(rec, req.WithContext(ctx), set, "page", "knowledge", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<title>Knowledge · Enclava</title>`,
		`<link rel="stylesheet" href="/dashboard/static/app.css">`,
		`/dashboard/static/htmx.min.js`,
		`/dashboard/static/app.js`,
		`<body class="" data-caps="chat search">`, // no theme cookie → dark default
		`class="skip"`,
		`<aside class="sidebar" id="sidebar">`,
		`<nav class="navgroups" id="nav">`,
		`class="navlink active" href="/dashboard/knowledge"`,
		`aria-current="page"`,
		`class="conf-card none"`,
		`No confidential endpoints`,
		`data-action="conf-pop"`,
		`class="userrow"`,
		`<b>user-1</b>`,
		`id="drawer"`, `id="backdrop"`, `id="toast"`, `id="pop"`,
		`id="conf-pop-tpl"`, `id="user-menu-tpl"`,
		`<h1>Probe page</h1>`,
		`>Enclava</p>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("shell body missing %q", want)
		}
	}
	// A capability-filtered nav must not render links for missing caps, and a
	// member never sees the admin lockhint.
	if strings.Contains(body, `href="/dashboard/extract"`) || strings.Contains(body, "Visible to admins") {
		t.Errorf("nav rendered content the principal's caps hide")
	}
	if !strings.Contains(body, `href="/dashboard/search"`) {
		t.Errorf("granted capability's nav link missing")
	}

	// Every capability → all three groups with the admin lockhint present.
	adminCtx := withCaps(auth.WithPrincipal(req.Context(), auth.Principal{TenantID: "tenant-1"}),
		dashboardCaps{Chat: true, Search: true, KnowledgeManage: true, SourcesManage: true, Extract: true,
			ExtractManage: true, AssistantsManage: true, ToolsManage: true, ModelsManage: true,
			AccessManage: true, BillingView: true})
	rec = httptest.NewRecorder()
	app.renderDashboardInto(rec, req.WithContext(adminCtx), set, "page", "knowledge", nil)
	body = rec.Body.String()
	if !strings.Contains(body, `title="Visible to admins"`) {
		t.Errorf("admin nav missing the lockhint")
	}
	if strings.Count(body, `<div class="group-title">`) != 3 {
		t.Errorf("admin nav groups = %d, want 3", strings.Count(body, `<div class="group-title">`))
	}
}

func TestRenderDashboardBufferRule(t *testing.T) {
	base, err := dashboardBaseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	// A page whose content errors MID-render: bytes already written to the
	// buffer must never reach the client with a 200 (P7-10).
	broken, _ := base.Clone()
	if _, err := broken.Parse(`{{define "content"}}<h1>partial</h1>{{index .Data "boom"}}{{end}}`); err != nil {
		t.Fatal(err)
	}
	app := &App{}
	req := httptest.NewRequest("GET", "/dashboard/broken", nil)
	rec := httptest.NewRecorder()
	app.renderDashboardInto(rec, req, broken, "page", "broken", "not-a-map")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "partial") {
		t.Fatalf("partial page bytes committed with 500: %q", body)
	}

	// Fragment renders obey the same rule.
	frag, _ := base.Clone()
	if _, err := frag.Parse(`{{define "frag-row"}}<tr><td>ok</td></tr>{{end}}`); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	app.renderDashboardInto(rec, req, frag, "frag-row", "broken", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<td>ok</td>") {
		t.Fatalf("fragment render = %d %q, want 200 with row", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("fragment content-type = %q", ct)
	}
}

func TestRenderDashboardMissingPage500(t *testing.T) {
	app := &App{}
	req := httptest.NewRequest("GET", "/dashboard/ghost", nil)
	rec := httptest.NewRecorder()
	app.renderDashboard(rec, req, "ghost", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("missing page status = %d, want 500 (D-22: never falls back)", rec.Code)
	}
}

func TestThemeFromRequest(t *testing.T) {
	req := httptest.NewRequest("GET", "/dashboard", nil)
	if got := themeFromRequest(req); got != "" {
		t.Fatalf("no cookie → %q, want dark default", got)
	}
	req.AddCookie(&http.Cookie{Name: "enclava_theme", Value: "light"})
	if got := themeFromRequest(req); got != "light" {
		t.Fatalf("light cookie → %q, want light", got)
	}
	req = httptest.NewRequest("GET", "/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: "enclava_theme", Value: "sepia"})
	if got := themeFromRequest(req); got != "" {
		t.Fatalf("unknown value → %q, want dark default", got)
	}
}

func TestDashboardViewMeta(t *testing.T) {
	base, err := dashboardBaseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	set, _ := base.Clone()
	if _, err := set.Parse(`{{define "content"}}{{.PageTitle}}{{end}}`); err != nil {
		t.Fatal(err)
	}
	app := &App{}
	req := httptest.NewRequest("GET", "/dashboard/meta", nil).WithContext(context.Background())
	rec := httptest.NewRecorder()
	app.renderDashboardInto(rec, req, set, "page", "meta", nil)
	if body := rec.Body.String(); !strings.Contains(body, `id="content">Meta</div>`) {
		t.Fatalf("fallback PageTitle %q missing humanized page name", rec.Body.String())
	}
}
