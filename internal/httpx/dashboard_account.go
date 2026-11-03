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
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/store/postgres"
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
	Dev             accountDev
}

// accountDev is the developer-card model (P-12/UX-M03). Visible is decided
// server-side; the template renders branches only. Keys are strictly the
// caller's own rows — never a tenant-wide list — and carry tokens, never
// cost.
type accountDev struct {
	Visible        bool
	CanManageKeys  bool // access.manage → key management link; else the request affordance
	BaseURL        string
	Keys           []postgres.APIKeyUsage
	KeysErr        bool
	RequestPending bool
	RequestedAt    *time.Time
}

func (d accountData) DashboardTitle() string  { return "Account" }
func (d accountData) DashboardModule() string { return "account" }

// registerDashboardAccountRoutes mounts the Account page, its sessions
// fragment, the in-app API reference and the two developer-card mutations —
// every route behind the session+CSRF chain; access-request dismiss adds
// the access.manage gate.
func (a *App) registerDashboardAccountRoutes() {
	a.mux.HandleFunc("/dashboard/account", a.sessionAuth.capSession(a.handleDashboardAccountPage))
	a.mux.HandleFunc("/dashboard/partials/account/sessions", a.sessionAuth.capSession(a.handleDashboardAccountSessions))
	a.mux.HandleFunc("/dashboard/api", a.sessionAuth.requireCap(capAPI, a.handleDashboardAPIReference))
	a.mux.HandleFunc("/dashboard/account/api-access-request", a.sessionAuth.capSession(a.handleDashboardAPIAccessRequest))
	a.mux.HandleFunc("/dashboard/access-requests/{requestID}/dismiss", a.sessionAuth.requireCap(capAccessManage, a.handleDashboardAPIAccessDismiss))
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
	data.Dev = a.accountDevFor(r, p, user)
	return data
}

// accountDevFor composes the developer card: the caller's own keys with
// settlement-derived token totals, the pending-request state and the
// key-management affordance. The card shows to API callers, key managers
// and anyone who owns a key or already asked for one — a member with no
// API relationship never sees it.
func (a *App) accountDevFor(r *http.Request, p auth.Principal, user auth.User) accountDev {
	caps, _ := capsFromContext(r.Context())
	dev := accountDev{BaseURL: requestOrigin(r, a.trustProxy), CanManageKeys: caps.AccessManage}
	if a.identity == nil {
		dev.KeysErr = true
		dev.Visible = caps.API || dev.CanManageKeys
		return dev
	}
	keys, err := a.identity.ListAPIKeyUsageForOwner(r.Context(), p.TenantID, user.ID)
	if err != nil {
		log.Printf("account own keys for %s: %v", user.ID, err)
		dev.KeysErr = true
	} else {
		dev.Keys = keys
	}
	if req, err := a.identity.OpenAPIAccessRequest(r.Context(), p.TenantID, user.ID); err == nil {
		dev.RequestPending = true
		dev.RequestedAt = &req.CreatedAt
	} else if !errors.Is(err, auth.ErrNotFound) {
		log.Printf("account api access request for %s: %v", user.ID, err)
	}
	dev.Visible = caps.API || dev.CanManageKeys || len(dev.Keys) > 0 || dev.RequestPending
	return dev
}

// apiReferenceData backs the in-app API reference — a curated, static page;
// the only live value is the deployment's own base URL.
type apiReferenceData struct {
	BaseURL string
}

func (d apiReferenceData) DashboardTitle() string  { return "API reference" }
func (d apiReferenceData) DashboardModule() string { return "" }

// handleDashboardAPIReference renders the curated reference behind the same
// capability the nav entry and developer card use — no API scope, no docs.
func (a *App) handleDashboardAPIReference(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	a.renderDashboard(w, r, "api", apiReferenceData{BaseURL: requestOrigin(r, a.trustProxy)})
}

// apiAccessRequestBody is the request card's JSON payload.
type apiAccessRequestBody struct {
	Note string `json:"note"`
}

// handleDashboardAPIAccessRequest records the caller's request for an API
// key. Any signed-in user may ask; the store enforces one open request per
// user and nothing is granted — an administrator issues keys only.
func (a *App) handleDashboardAPIAccessRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteMethodNotAllowed(w)
		return
	}
	if a.identity == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "identity management not configured", "server_error", "not_configured")
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	user, err := a.accountUser(r, p)
	if err != nil {
		if errors.Is(err, auth.ErrNotFound) {
			WriteOpenAIError(w, http.StatusForbidden, "a user identity is required", "authentication_error", "forbidden")
			return
		}
		log.Printf("api access request user lookup: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "access_request_failed")
		return
	}
	var body apiAccessRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		WriteOpenAIError(w, http.StatusBadRequest, "invalid request body", "invalid_request_error", "invalid_request")
		return
	}
	note := strings.TrimSpace(body.Note)
	if len([]rune(note)) > 500 {
		WriteOpenAIError(w, http.StatusBadRequest, "note must be at most 500 characters", "invalid_request_error", "invalid_request")
		return
	}
	created, err := a.identity.RequestAPIAccess(r.Context(), p.TenantID, NewID("req"), user.ID, note, time.Now().UTC())
	if err != nil {
		log.Printf("api access request insert for %s: %v", user.ID, err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "access_request_failed")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"status": "open", "created": created})
}

// handleDashboardAPIAccessDismiss closes an open request without granting
// anything — the admin's "seen it" path from the Home attention row.
func (a *App) handleDashboardAPIAccessDismiss(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteMethodNotAllowed(w)
		return
	}
	if a.identity == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "identity management not configured", "server_error", "not_configured")
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	err := a.identity.DismissAPIAccessRequest(r.Context(), p.TenantID, r.PathValue("requestID"), p.ActorID, time.Now().UTC())
	if err != nil {
		if errors.Is(err, auth.ErrNotFound) {
			WriteOpenAIError(w, http.StatusNotFound, "request not found", "invalid_request_error", "not_found")
			return
		}
		log.Printf("api access request dismiss: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "access_request_dismiss_failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
