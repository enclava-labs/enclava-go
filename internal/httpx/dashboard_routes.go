// dashboard_routes.go — the guard primitives every rebuilt dashboard page
// registers through (D-18): capSession requires a verified session and
// stashes the request's capabilities; requireCap adds the single capability
// gate. Guard order is session → CSRF (both inside RequireSession) → cap.
package httpx

import (
	"net/http"
	"strings"

	"enclava-go/internal/auth"
)

// capSession gates a handler on a verified session principal (GETs redirect
// to login, mutations still pass RequireSession's CSRF check) and stashes
// the principal's capabilities for session-only pages (Home, Account) and
// the error renderers.
func (s *SessionAuth) capSession(next http.HandlerFunc) http.HandlerFunc {
	return s.RequireSession(func(w http.ResponseWriter, r *http.Request) {
		// RequireSession guarantees the principal; the zero value keeps the
		// stash fail-closed if a context is ever built without one.
		principal, _ := auth.PrincipalFromContext(r.Context())
		next(w, r.WithContext(withCaps(r.Context(), dashboardCapabilities(principal))))
	})
}

// requireCap composes capSession with the named capability check: a missing
// capability denies page requests with the 403 page and partial/HX requests
// with an empty-body 403.
func (s *SessionAuth) requireCap(c dashboardCap, next http.HandlerFunc) http.HandlerFunc {
	return s.capSession(func(w http.ResponseWriter, r *http.Request) {
		caps, ok := capsFromContext(r.Context())
		if !ok || !caps.grants(c) {
			if isDashboardPartial(r) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			s.renderDashboardForbidden(w, r)
			return
		}
		next(w, r)
	})
}

// isDashboardPartial reports whether the response must stay fragment-sized:
// paths under /dashboard/partials/ (covers bare fetches) or any htmx call.
func isDashboardPartial(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/dashboard/partials/") || r.Header.Get("HX-Request") != ""
}
