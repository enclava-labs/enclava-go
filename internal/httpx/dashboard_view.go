// dashboard_view.go — the shell's common view model and the render entry
// every new page goes through (OA-09/OA-10). Templates execute into a buffer
// first (P7-10): a mid-content failure must never commit a partial page with
// a 200 — the caller sees a 500 whose log names the page (D-22).
package httpx

import (
	"bytes"
	"html/template"
	"log"
	"net/http"
	"strings"

	"enclava-go/internal/auth"
)

// dashboardPageMeta is optional on a page's data model: it sets the <title>
// and the page JS module. Either method may return "".
type dashboardPageMeta interface {
	DashboardTitle() string
	DashboardModule() string
}

// dashboardView is the one model every app-template page executes with.
type dashboardView struct {
	PageTitle string        // <title>; dashboardPageMeta or humanized page name
	Module    string        // page JS module: /dashboard/static/pages/<Module>.js
	Can       dashboardCaps // from capsFromContext; zero value → all false
	Data      any           // the page handler's own model
	Nav       []dashboardNavGroup
	Active    string // nav item ID for the active page (= page name)
	Conf      confidentialSummary
	User      dashboardUser
	Org       string // tenant display name; falls back to the tenant slug
	Theme     string // "light" when the enclava_theme cookie says so; dark ""
	CSRF      string // session CSRF token; fills the logout form's hidden input
}

// CapsAttr renders the granted capability names for the body's data-caps
// hook (space-separated §3.1 names).
func (v dashboardView) CapsAttr() string {
	names := make([]string, 0, 11)
	for _, c := range []dashboardCap{capChat, capSearch, capKnowledgeManage, capSourcesManage,
		capExtract, capExtractManage, capAssistantsManage, capToolsManage,
		capModelsManage, capAccessManage, capBillingView} {
		if v.Can.grants(c) {
			names = append(names, string(c))
		}
	}
	return strings.Join(names, " ")
}

// themeFromRequest reads the theme cookie the theme toggle writes: "light",
// or "" for the dark default (anything else stays dark).
func themeFromRequest(r *http.Request) string {
	if c, err := r.Cookie("enclava_theme"); err == nil && c.Value == "light" {
		return "light"
	}
	return ""
}

// buildDashboardView assembles the full view; fragments get the same view so
// .Can and the shared fields stay uniform everywhere.
func (a *App) buildDashboardView(r *http.Request, page string, data any) dashboardView {
	ctx := r.Context()
	caps, _ := capsFromContext(ctx)
	p, _ := auth.PrincipalFromContext(ctx)
	v := dashboardView{
		Can:    caps,
		Data:   data,
		Nav:    dashboardNav(caps, page),
		Active: page,
		Conf:   a.confidentialSummary(ctx, p, caps),
		User:   a.dashboardUserFor(ctx, p),
		Org:    a.dashboardOrgName(ctx, p.TenantID),
		Theme:  themeFromRequest(r),
		CSRF:   csrfCookieValue(r),
	}
	if meta, ok := data.(dashboardPageMeta); ok {
		v.PageTitle = meta.DashboardTitle()
		v.Module = meta.DashboardModule()
	}
	if v.PageTitle == "" {
		v.PageTitle = dashboardHumanize(page)
	}
	return v
}

// renderDashboard renders the page's full shell. page names BOTH the template
// file (pages/<page>.tmpl) and the nav ID; sub-pages reuse the parent's page
// file and branch on the path. Never falls back to another page.
func (a *App) renderDashboard(w http.ResponseWriter, r *http.Request, page string, data any) {
	set, err := dashboardTemplateSet(page)
	if err != nil {
		a.renderDashboardTemplateError(w, page, err)
		return
	}
	a.renderDashboardInto(w, r, set, "page", page, data)
}

// renderDashboardFragment renders one named fragment (frag-<name>) with the
// full view — same buffer-first rule as the page (P7-10).
func (a *App) renderDashboardFragment(w http.ResponseWriter, r *http.Request, page, name string, data any) {
	set, err := dashboardTemplateSet(page)
	if err != nil {
		a.renderDashboardTemplateError(w, page, err)
		return
	}
	a.renderDashboardInto(w, r, set, "frag-"+name, page, data)
}

// renderDashboardInto executes the named template into a buffer and commits
// only on success. Tests drive this directly with a parsed set so the real
// layout/components render without adding fixture pages to the embed FS.
func (a *App) renderDashboardInto(w http.ResponseWriter, r *http.Request, set *template.Template, tmplName, page string, data any) {
	view := a.buildDashboardView(r, page, data)
	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, tmplName, view); err != nil {
		a.renderDashboardTemplateError(w, page, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = buf.WriteTo(w)
}

func (a *App) renderDashboardTemplateError(w http.ResponseWriter, page string, err error) {
	// D-22: the log names the page; the response is a bare 500 — never a
	// partial page, never a fallback page.
	log.Printf("dashboard page %q: %v", page, err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}
