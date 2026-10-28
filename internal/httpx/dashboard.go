package httpx

import (
	"embed"
	"io/fs"
	"net/http"

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

type dashboardLoginData struct {
	Email            string
	Error            string
	ReturnTo         string
	Providers        []auth.OIDCProvider
	LinkToken        string
	LinkProviderName string
	LinkEmail        string
}

// registerLegacyDashboardRoutes kept only as the hook point — every
// pre-phase-7 page route died with its replacement (phase-9 sweep 09-10 +
// phase-10 Home cutover). It remains so future legacy stragglers have a
// documented home and the call site stays stable.
func (a *App) registerLegacyDashboardRoutes() {}

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
