// dashboard_auth_pages.go — phase-7 auth/error page views (P1.6): the
// standalone login/403/404 documents render through dashboardAuthTemplates.
// This file owns the Org/Theme view wrappers around the existing
// dashboardLoginData, the SessionAuth org humanization and the error-view
// assembly — nothing else. The App shell resolves the real tenant display
// name (GetTenantDisplayName); SessionAuth holds no identity repo.
package httpx

import (
	"context"
	"log"
	"net/http"
	"strings"

	"enclava-go/internal/auth"
)

// dashboardLoginData is the login-form model SessionAuth builds at each
// failure/render site; dashboardLoginView wraps it for auth/login.tmpl.
type dashboardLoginData struct {
	Email            string
	Error            string
	ReturnTo         string
	Providers        []auth.OIDCProvider
	LinkToken        string
	LinkProviderName string
	LinkEmail        string
}

// dashboardLoginView wraps dashboardLoginData for auth/login.tmpl: the
// standalone document adds the org name and theme class without touching the
// shared login-data type or any of its builders.
type dashboardLoginView struct {
	dashboardLoginData
	Org   string
	Theme string
}

// dashboardErrorView is the whole errors.tmpl data model: fixed copy, org
// name and theme only — no caps/nav, the pages must render for anonymous
// visitors and for callers whose capabilities are absent.
type dashboardErrorView struct {
	Org   string
	Theme string
}

// orgName humanizes the tenant slug for the standalone auth pages
// ("northwind" → "Northwind"), falling back to the product name. The ctx
// parameter keeps the helper signature ready for the real display-name
// lookup without a second call-site migration.
func (s *SessionAuth) orgName(_ context.Context) string {
	slug := strings.TrimSpace(s.tenantSlug)
	if slug == "" {
		return "Enclava"
	}
	words := strings.Fields(strings.NewReplacer("-", " ", "_", " ").Replace(slug))
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	if name := strings.Join(words, " "); name != "" {
		return name
	}
	return "Enclava"
}

// themeClass maps the enclava_theme cookie (written by app.js's theme action)
// to the body class of the standalone pages; dark is the default.
func themeClass(r *http.Request) string {
	if c, err := r.Cookie("enclava_theme"); err == nil && c.Value == "light" {
		return "light"
	}
	return ""
}

// renderDashboardError writes the designed standalone error document: phase-6
// status/header order is preserved and only the markup changed.
func (s *SessionAuth) renderDashboardError(w http.ResponseWriter, r *http.Request, status int, name string) {
	setNoStoreHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	view := dashboardErrorView{Org: s.orgName(r.Context()), Theme: themeClass(r)}
	if err := dashboardAuthTemplates().ExecuteTemplate(w, name, view); err != nil {
		log.Printf("failed to render dashboard %s page: %v", name, err)
	}
}
