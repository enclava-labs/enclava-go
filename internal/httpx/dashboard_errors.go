// dashboard_errors.go — the phase-6 guard entry points. Only the two frozen
// signatures live here; the bodies render the phase-7 designed pages through
// dashboardAuthTemplates (dashboard_auth_pages.go assembles the view).
package httpx

import "net/http"

// renderDashboardForbidden renders the 403 page for a signed-in caller
// lacking the capability a page requires.
func (s *SessionAuth) renderDashboardForbidden(w http.ResponseWriter, r *http.Request) {
	s.renderDashboardError(w, r, http.StatusForbidden, "error-403")
}

// renderDashboardNotFound renders the 404 page for unknown dashboard paths.
func (s *SessionAuth) renderDashboardNotFound(w http.ResponseWriter, r *http.Request) {
	s.renderDashboardError(w, r, http.StatusNotFound, "error-404")
}
