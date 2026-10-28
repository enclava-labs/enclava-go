// dashboard_errors.go — minimal self-contained 403/404 dashboard pages,
// runnable before the phase-7 theme exists. Phase 7 (P1.6) re-skins only the
// two bodies onto errors.tmpl; the signatures are frozen.
package httpx

import (
	"fmt"
	"net/http"
)

// renderDashboardForbidden renders the 403 page for a signed-in caller
// lacking the capability a page requires.
func (s *SessionAuth) renderDashboardForbidden(w http.ResponseWriter, r *http.Request) {
	s.renderDashboardError(w, r, http.StatusForbidden, "You don't have access to this page",
		`<a href="/dashboard">Home</a>`)
}

// renderDashboardNotFound renders the 404 page for unknown dashboard paths.
func (s *SessionAuth) renderDashboardNotFound(w http.ResponseWriter, r *http.Request) {
	s.renderDashboardError(w, r, http.StatusNotFound, "Page not found", "")
}

func (s *SessionAuth) renderDashboardError(w http.ResponseWriter, r *http.Request, status int, message, action string) {
	setNoStoreHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>%d Enclava</title></head>
<body><main><h1>%s</h1>%s</main></body>
</html>`, status, message, action)
}
