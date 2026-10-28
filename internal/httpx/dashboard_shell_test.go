// dashboard_shell_test.go — the registrar machinery: legacy + new routes
// coexist under one mux without duplicate-pattern panics, the moved static/auth
// routes stay registered, and self-registered page phases are invoked.
package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDashboardRouteRegistration(t *testing.T) {
	registered := false
	registerDashboardPage(func(a *App) {
		registered = true
		a.mux.HandleFunc("/dashboard/boats", func(w http.ResponseWriter, r *http.Request) {})
	})

	app := &App{mux: http.NewServeMux(), sessionAuth: &SessionAuth{}}
	app.registerDashboardRoutes() // a duplicate pattern would panic here
	if !registered {
		t.Fatal("registerDashboardRoutes did not invoke page registrars")
	}

	for _, path := range []string{
		"/dashboard/static/app.css", // static server (moved into dashboard_shell.go)
		"/dashboard/login",          // session auth (moved)
		"/dashboard/logout",         // session auth (moved)
		"/auth/oidc/link",           // link identity (moved)
		"/auth/oidc/x/start",        // suffix router (moved)
		"/dashboard",                // legacy exact Home
		"/dashboard/keys",           // legacy page
		"/dashboard/partials/stats", // legacy partial
		"/dashboard/boats",          // self-registered page
		"/dashboard/anything-else",  // /dashboard/ subtree catch-all (phase-6 404 body) still wins
	} {
		if _, pattern := app.mux.Handler(httptest.NewRequest("GET", path, nil)); pattern == "" {
			t.Errorf("%s: no handler registered", path)
		}
	}
	// A path outside the dashboard stays unhandled.
	if _, pattern := app.mux.Handler(httptest.NewRequest("GET", "/api/v1/none", nil)); pattern != "" {
		t.Errorf("/api/v1/none unexpectedly matched %q", pattern)
	}
}
