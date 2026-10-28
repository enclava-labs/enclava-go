// dashboard_shell.go — central dashboard registration (frozen in 07-CONTEXT):
// static + auth routes, then every page phase's self-registered registrar.
// Page phases call registerDashboardPage from init() and NEVER edit this file
// or app.go for routing; registration order is unimportant (ServeMux prefers
// more-specific patterns).
package httpx

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"enclava-go/internal/auth"
)

//go:embed dashboard/static/*
var dashboardStaticFS embed.FS

var dashboardStaticFiles fs.FS = func() fs.FS {
	files, err := fs.Sub(dashboardStaticFS, "dashboard/static")
	if err != nil {
		panic(err)
	}
	return files
}()

// dashboardPageRegistrars is appended by page files' init() through
// registerDashboardPage; registerDashboardRoutes iterates it.
var dashboardPageRegistrars []func(*App)

func registerDashboardPage(fn func(*App)) {
	dashboardPageRegistrars = append(dashboardPageRegistrars, fn)
}

// registerDashboardRoutes owns everything shared: static files, session auth
// endpoints, then the page registrars. Guards stay at each page's own
// HandleFunc sites (requireCap/capSession wrappers); the /dashboard/ subtree
// stays the 404 catch-all owned by phase 6.
func (a *App) registerDashboardRoutes() {
	a.mux.Handle("/dashboard/static/", http.StripPrefix("/dashboard/static/", http.FileServer(http.FS(dashboardStaticFiles))))
	a.mux.HandleFunc("/dashboard/login", a.sessionAuth.HandleLogin)
	a.mux.HandleFunc("/dashboard/logout", a.sessionAuth.HandleLogout)
	a.mux.HandleFunc("/auth/oidc/link", a.sessionAuth.HandleLinkIdentity)
	a.mux.HandleFunc("/auth/oidc/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/start"):
			a.sessionAuth.HandleOIDCStart(w, r)
		case strings.HasSuffix(r.URL.Path, "/callback"):
			a.sessionAuth.HandleOIDCCallback(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	for _, fn := range dashboardPageRegistrars {
		fn(a)
	}
}

// dashboardTenantID resolves the tenant for dashboard key/usage reads from
// the request's session-verified principal — RequireSession has already
// placed it on the context, so an absent principal is a truthful 401 rather
// than a fabricated tenant.
func (a *App) dashboardTenantID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if p, ok := auth.PrincipalFromContext(r.Context()); ok && p.TenantID != "" {
		return p.TenantID, true
	}
	WriteUnauthorized(w, "authenticated principal required")
	return "", false
}
