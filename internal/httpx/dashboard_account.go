// dashboard_account.go — Account (OA-30, source §6.13): the self-service half
// of the old /dashboard/identity page — profile, change password (current
// password re-proved server-side by the API), own sessions with revoke, and
// linked sign-ins with the OIDC link confirm whose password input carries
// name="password" (the OA-12/§6.13 unnamed-input fix carried forward). The
// one session-only page every role reaches; no admin surface renders here —
// identity administration lives in Access (09-07). Every write is the page
// module's fetch to the /api/v1/identities session+CSRF APIs (D-17).
package httpx

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"enclava-go/internal/auth"
)

func init() { registerDashboardPage((*App).registerDashboardAccountRoutes) }

// accountProfile is the profile card model — exactly what /me renders.
type accountProfile struct {
	Name, Email string
	Roles       []string
	RolesErr    bool // read failure renders truthful failure text, never an empty-state lie
}

// accountSessionRow projects one stored session. Revoked rows carry the
// vocabulary state "stopped" plus the stored revoke reason; active rows
// render no status pill (absent state — the vocabulary has no "active
// session" label).
type accountSessionRow struct {
	ID            string
	ShortID       string
	CreatedAt     time.Time
	IdleExpiresAt time.Time
	Revoked       bool
	Reason        string
	Current       bool
}

// accountIdentityRow is one linked external sign-in. Only issuer+subject are
// stored (no provider display name) — the issuer renders as its host.
type accountIdentityRow struct {
	Issuer, Subject string
	LinkedAt        time.Time
}

// accountData is the page model; the sessions fragment reuses the same struct
// so the htmx refresh and the first paint render identically.
type accountData struct {
	Profile         accountProfile
	Sessions        []accountSessionRow
	SessionsErr     bool
	SessionsEmpty   dashboardEmpty
	Identities      []accountIdentityRow
	IdentitiesErr   bool // read failures render truthful failure text
	IdentitiesEmpty dashboardEmpty
	Providers       []auth.OIDCProvider
	LinkToken       string // ?link_token= → the link confirm form renders
	OIDCReady       bool   // OIDC configured; gates the link-start actions
}

func (d accountData) DashboardTitle() string  { return "Account" }
func (d accountData) DashboardModule() string { return "account" }

// registerDashboardAccountRoutes mounts the Account page and its sessions
// fragment behind capSession — session-required, no feature capability
// (T-09-09-02: reads are strictly the caller's own records).
func (a *App) registerDashboardAccountRoutes() {
	a.mux.HandleFunc("/dashboard/account", a.sessionAuth.capSession(a.handleDashboardAccountPage))
	a.mux.HandleFunc("/dashboard/partials/account/sessions", a.sessionAuth.capSession(a.handleDashboardAccountSessions))
}

// handleDashboardAccountPage renders the four §6.13 sections in order:
// Profile, Change password, Sessions, Linked sign-ins (+ link confirm when a
// link_token is present).
func (a *App) handleDashboardAccountPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	user, err := a.accountUser(r, p)
	if err != nil {
		a.writeAccountUserError(w, r, err)
		return
	}
	data := a.accountDataFor(r, p, user)
	if q := r.URL.Query().Get("link_token"); strings.TrimSpace(q) != "" {
		data.LinkToken = q
	}
	a.renderDashboard(w, r, "account", data)
}

// handleDashboardAccountSessions re-renders the sessions fragment — the
// shared refresh event after a revoke (Enclava.refresh) re-fetches it.
func (a *App) handleDashboardAccountSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	user, err := a.accountUser(r, p)
	if err != nil {
		a.writeAccountUserError(w, r, err)
		return
	}
	data := accountData{SessionsEmpty: dashboardEmpty{Icon: "clock", Text: "No sessions."}}
	if sessions, err := a.identity.ListSessionsForUser(r.Context(), p.TenantID, user.ID); err != nil {
		log.Printf("account sessions for %s: %v", user.ID, err)
		data.SessionsErr = true
	} else {
		data.Sessions = accountSessionRows(sessions, p.SessionID)
	}
	a.renderDashboardFragment(w, r, "account", "account-sessions", data)
}

// accountDataFor performs the page's UserID-scoped reads: roles, sessions,
// linked identities and the enabled OIDC providers a link can start from.
// Read failures degrade to truthful failure text so one bad read never
// blanks the whole self-service page.
func (a *App) accountDataFor(r *http.Request, p auth.Principal, user auth.User) accountData {
	data := accountData{
		Profile:         accountProfile{Name: user.DisplayName, Email: user.Email},
		OIDCReady:       a.sessionAuth != nil && a.sessionAuth.oidc != nil,
		SessionsEmpty:   dashboardEmpty{Icon: "clock", Text: "No sessions."},
		IdentitiesEmpty: dashboardEmpty{Icon: "link", Text: "No external sign-ins are linked to this account."},
	}
	if data.Profile.Name == "" {
		data.Profile.Name = user.Email
	}
	roles, err := a.identity.RolesForUser(r.Context(), p.TenantID, user.ID)
	if err != nil {
		log.Printf("account roles for %s: %v", user.ID, err)
		data.Profile.RolesErr = true
	} else {
		data.Profile.Roles = make([]string, 0, len(roles))
		for _, ro := range roles {
			data.Profile.Roles = append(data.Profile.Roles, ro.Name)
		}
	}
	if sessions, err := a.identity.ListSessionsForUser(r.Context(), p.TenantID, user.ID); err != nil {
		log.Printf("account sessions for %s: %v", user.ID, err)
		data.SessionsErr = true
	} else {
		data.Sessions = accountSessionRows(sessions, p.SessionID)
	}
	links, err := a.identity.ListAuthIdentitiesForUser(r.Context(), p.TenantID, user.ID)
	if err != nil {
		log.Printf("account identities for %s: %v", user.ID, err)
		data.IdentitiesErr = true
	} else {
		data.Identities = make([]accountIdentityRow, 0, len(links))
		for _, l := range links {
			data.Identities = append(data.Identities, accountIdentityRow{
				Issuer: accountIssuerLabel(l.Issuer), Subject: l.Subject, LinkedAt: l.LinkedAt,
			})
		}
	}
	if data.OIDCReady {
		if providers, err := a.sessionAuth.oidc.EnabledProviders(r.Context(), p.TenantID); err != nil {
			log.Printf("account providers: %v", err)
		} else {
			data.Providers = providers
		}
	}
	return data
}

// accountUser resolves the session principal's stored user record — the read
// /api/v1/identities/me performs. A principal without a user record fails
// exactly like the API's own "a user identity is required" guard.
func (a *App) accountUser(r *http.Request, p auth.Principal) (auth.User, error) {
	if a.identity == nil || p.UserID == "" {
		return auth.User{}, auth.ErrNotFound
	}
	return a.identity.GetUser(r.Context(), p.TenantID, p.UserID)
}

// writeAccountUserError maps the lookup failure onto the page surface: no
// user record → the designed 403 page (bare 403 for fragment requests,
// mirroring requireCap), anything else → 500.
func (a *App) writeAccountUserError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, auth.ErrNotFound) {
		if isDashboardPartial(r) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		a.sessionAuth.renderDashboardForbidden(w, r)
		return
	}
	log.Printf("account page user lookup: %v", err)
	WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "account_page_failed")
}

// accountSessionRows marks the caller's own session the same way the
// sessions API does (redactSession's `current`).
func accountSessionRows(sessions []auth.Session, currentID string) []accountSessionRow {
	rows := make([]accountSessionRow, 0, len(sessions))
	for _, s := range sessions {
		rows = append(rows, accountSessionRow{
			ID:            s.ID,
			ShortID:       accountShortID(s.ID),
			CreatedAt:     s.CreatedAt,
			IdleExpiresAt: s.IdleExpiresAt,
			Revoked:       s.RevokedAt != nil,
			Reason:        s.RevokeReason,
			Current:       s.ID == currentID,
		})
	}
	return rows
}

// accountShortID trims the id prefix for display; the full id stays in the
// revoke control's data attribute.
func accountShortID(id string) string {
	if i := strings.Index(id, "_"); i >= 0 && i+1 < len(id) {
		id = id[i+1:]
	}
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

// accountIssuerLabel renders a stored issuer URL as its host so identity rows
// read "accounts.example.com", never a raw URL blob.
func accountIssuerLabel(issuer string) string {
	if u, err := url.Parse(issuer); err == nil && u.Host != "" {
		return u.Host
	}
	return issuer
}
