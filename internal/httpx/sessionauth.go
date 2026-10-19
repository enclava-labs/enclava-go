// sessionauth.go — browser login handlers on server-side revocable sessions
// (AUTH-03): local credential login, logout with server-side revocation and
// the OIDC start/callback/link endpoints. Constructed in main and mounted on
// the mux by the dashboard wiring; every path that mints a session runs a
// verified credential first — no path issues a session for an unverified
// identity.
package httpx

import (
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"enclava-go/internal/auth"
)

const (
	sessionCookieName     = "enclava_session"
	sessionCSRFCookieName = "enclava_csrf"
)

// SessionAuth serves dashboard login/logout/OIDC. It resolves the tenant
// UUID from the configured slug per request so tenant provisioning order
// never races startup construction.
type SessionAuth struct {
	auth       *auth.Service
	oidc       *auth.OIDCService
	sessionTTL time.Duration
	templates  *template.Template
	tenantSlug string
}

// NewSessionAuth builds the handler set. oidc may be nil — provider links
// then never render and the OIDC handlers deny. tenantSlug identifies the
// tenant the browser login targets.
func NewSessionAuth(authSvc *auth.Service, oidc *auth.OIDCService, sessionTTL time.Duration, templates *template.Template, tenantSlug string) *SessionAuth {
	return &SessionAuth{
		auth:       authSvc,
		oidc:       oidc,
		sessionTTL: sessionTTL,
		templates:  templates,
		tenantSlug: strings.TrimSpace(tenantSlug),
	}
}

func (s *SessionAuth) tenantID(r *http.Request) (string, error) {
	return s.auth.ResolveTenantID(r.Context(), s.tenantSlug)
}

func (s *SessionAuth) renderLogin(w http.ResponseWriter, r *http.Request, data dashboardLoginData) {
	setNoStoreHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "login.tmpl", data); err != nil {
		log.Printf("failed to render login page: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "login_render_failed")
	}
}

// HandleLogin serves GET (render) and POST (local credential login).
func (s *SessionAuth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleLoginGet(w, r)
	case http.MethodPost:
		s.handleLoginPost(w, r)
	default:
		WriteMethodNotAllowed(w)
	}
}

func (s *SessionAuth) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	if principal, ok := s.SessionPrincipal(r); ok && principal.UserID != "" {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	returnTo := returnToParam(r)
	data := dashboardLoginData{ReturnTo: returnTo}
	if s.oidc != nil {
		if tenantID, err := s.tenantID(r); err == nil {
			if providers, err := s.oidc.EnabledProviders(r.Context(), tenantID); err == nil {
				data.Providers = providers
			}
		}
	}
	s.renderLogin(w, r, data)
}

func (s *SessionAuth) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, r, dashboardLoginData{Error: "Invalid form submission"})
		return
	}
	email := strings.TrimSpace(r.FormValue("email"))
	returnTo := r.FormValue("return_to")
	if !safeLocalRedirect(returnTo) {
		s.renderLogin(w, r, dashboardLoginData{Email: email, Error: "Invalid return target"})
		return
	}
	tenantID, err := s.tenantID(r)
	if err != nil {
		s.renderLogin(w, r, dashboardLoginData{Email: email, Error: "Sign-in is not available"})
		return
	}
	principal, err := s.auth.AuthenticatePassword(r.Context(), tenantID, email, r.FormValue("password"))
	if err != nil {
		s.renderLogin(w, r, dashboardLoginData{
			Email: email, ReturnTo: returnTo,
			Error: "Invalid email or password",
		})
		return
	}
	s.issueSessionCookies(w, r, principal.TenantID, principal.UserID)
	target := "/dashboard"
	if returnTo != "" {
		target = returnTo
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// HandleLogout revokes the session server-side, then clears the cookies.
// Cookie-authenticated mutation → the session's CSRF token is required.
func (s *SessionAuth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteMethodNotAllowed(w)
		return
	}
	principal, ok := s.SessionPrincipal(r)
	if !ok {
		http.Redirect(w, r, "/dashboard/login", http.StatusSeeOther)
		return
	}
	if err := s.auth.ValidateCSRF(r.Context(), principal.TenantID, principal.SessionID,
		csrfFromRequest(r)); err != nil {
		WriteUnauthorized(w, "invalid CSRF token")
		return
	}
	if err := s.auth.RevokeSession(r.Context(), principal.TenantID, principal.SessionID, "logout"); err != nil {
		log.Printf("revoke session %s: %v", principal.SessionID, err)
	}
	s.clearSessionCookies(w, r)
	http.Redirect(w, r, "/dashboard/login", http.StatusSeeOther)
}

// HandleOIDCStart begins the provider flow and redirects to the authorize
// URL. Path shape: /auth/oidc/{provider}/start.
func (s *SessionAuth) HandleOIDCStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	if s.oidc == nil {
		WriteUnauthorized(w, "OIDC login is not configured")
		return
	}
	providerID := oidcProviderSegment(r)
	returnTo := r.URL.Query().Get("return_to")
	tenantID, err := s.tenantID(r)
	if err != nil {
		WriteUnauthorized(w, "sign-in is not available")
		return
	}
	flow, err := s.oidc.BeginFlow(r.Context(), tenantID, providerID, returnTo, externalBase(r))
	if err != nil {
		log.Printf("oidc begin for provider %q: %v", providerID, err)
		s.renderLogin(w, r, dashboardLoginData{Error: "OIDC sign-in could not start"})
		return
	}
	http.Redirect(w, r, flow.AuthURL, http.StatusSeeOther)
}

// HandleOIDCCallback completes the flow: a linked identity issues a real
// session; an unlinked one renders the link confirmation step on the login
// page — no session until the account is explicitly bound.
func (s *SessionAuth) HandleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	if s.oidc == nil {
		WriteUnauthorized(w, "OIDC login is not configured")
		return
	}
	setNoStoreHeaders(w)
	tenantID, err := s.tenantID(r)
	if err != nil {
		WriteUnauthorized(w, "sign-in is not available")
		return
	}
	res, err := s.oidc.CompleteFlow(r.Context(), tenantID,
		r.URL.Query().Get("state"), r.URL.Query().Get("code"), externalBase(r))
	if err != nil {
		log.Printf("oidc callback: %v", err)
		s.renderLogin(w, r, dashboardLoginData{Error: "OIDC sign-in failed"})
		return
	}
	if res.LinkRequired {
		s.renderLogin(w, r, dashboardLoginData{
			ReturnTo:         res.ReturnTo,
			LinkToken:        res.LinkToken,
			LinkProviderName: res.ProviderName,
			LinkEmail:        res.Email,
		})
		return
	}
	s.issueSessionCookies(w, r, tenantID, res.UserID)
	target := "/dashboard"
	if res.ReturnTo != "" {
		target = res.ReturnTo
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// HandleLinkIdentity redeems a pending link challenge: the browser proves
// the local account (email + password) and the verified issuer+subject is
// recorded with linked_by before a session is issued.
func (s *SessionAuth) HandleLinkIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteMethodNotAllowed(w)
		return
	}
	if s.oidc == nil {
		WriteUnauthorized(w, "OIDC login is not configured")
		return
	}
	setNoStoreHeaders(w)
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, r, dashboardLoginData{Error: "Invalid form submission"})
		return
	}
	linkToken := r.FormValue("link_token")
	email := strings.TrimSpace(r.FormValue("email"))
	returnTo := r.FormValue("return_to")
	tenantID, err := s.tenantID(r)
	if err != nil {
		WriteUnauthorized(w, "sign-in is not available")
		return
	}
	res, err := s.oidc.ConfirmLink(r.Context(), tenantID, linkToken, email, r.FormValue("password"))
	if err != nil {
		s.renderLogin(w, r, dashboardLoginData{
			LinkToken: linkToken, LinkEmail: email, ReturnTo: returnTo,
			Error: "Account linking failed — check the account credentials",
		})
		return
	}
	s.issueSessionCookies(w, r, tenantID, res.UserID)
	target := "/dashboard"
	if res.ReturnTo != "" {
		target = res.ReturnTo
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// SessionPrincipal resolves the request's session cookie to a verified
// principal — the tenant comes only from the verified session row.
func (s *SessionAuth) SessionPrincipal(r *http.Request) (auth.Principal, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return auth.Principal{}, false
	}
	principal, err := s.auth.AuthenticateSession(r.Context(), cookie.Value)
	if err != nil {
		return auth.Principal{}, false
	}
	return principal, true
}

// RequireSession gates a handler on a verified session principal: GET
// redirects to login; API/HTMX requests deny with 401. The principal is
// placed on the request context for downstream handlers.
func (s *SessionAuth) RequireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := s.SessionPrincipal(r)
		if !ok {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/dashboard/login", http.StatusSeeOther)
				return
			}
			WriteUnauthorized(w, "dashboard login required")
			return
		}
		next(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	}
}

// issueSessionCookies mints the server-side session and sets the opaque
// HttpOnly cookie plus a script-readable CSRF cookie — never a signed
// client-side value.
func (s *SessionAuth) issueSessionCookies(w http.ResponseWriter, r *http.Request, tenantID, userID string) {
	token, csrf, _, err := s.auth.IssueSession(r.Context(), tenantID, userID, s.sessionTTL)
	if err != nil {
		log.Printf("issue session for %s: %v", userID, err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "session_issue_failed")
		return
	}
	secure := r.TLS != nil
	expires := time.Now().Add(s.sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
	// The CSRF cookie is script-readable on purpose: page JS echoes it into
	// X-CSRF-Token for mutations; the server compares against the stored
	// per-session token, so the cookie alone never authenticates a mutation.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCSRFCookieName,
		Value:    csrf,
		Path:     "/",
		Expires:  expires,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

func (s *SessionAuth) clearSessionCookies(w http.ResponseWriter, r *http.Request) {
	secure := r.TLS != nil
	for _, name := range []string{sessionCookieName, sessionCSRFCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			Expires:  time.Unix(0, 0),
			HttpOnly: name == sessionCookieName,
			SameSite: http.SameSiteLaxMode,
			Secure:   secure,
		})
	}
}

// csrfFromRequest reads the CSRF token from the mutation's header, falling
// back to the script-readable cookie.
func csrfFromRequest(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-CSRF-Token")); v != "" {
		return v
	}
	if cookie, err := r.Cookie(sessionCSRFCookieName); err == nil {
		return cookie.Value
	}
	return ""
}

// returnToParam reads + allowlist-checks the ?return_to query param.
func returnToParam(r *http.Request) string {
	target := r.URL.Query().Get("return_to")
	if safeLocalRedirect(target) {
		return target
	}
	return ""
}

// safeLocalRedirect mirrors the service's return_to allowlist: local
// absolute paths only, no scheme/host/protocol-relative target.
func safeLocalRedirect(target string) bool {
	if target == "" {
		return true
	}
	if strings.HasPrefix(target, "//") || strings.ContainsAny(target, "\r\n") {
		return false
	}
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	return !u.IsAbs() && u.Host == "" && strings.HasPrefix(target, "/")
}

// externalBase is the deployment's external origin as the browser sees it —
// the callback URI the provider must return to.
func externalBase(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

// oidcProviderSegment extracts {provider} from /auth/oidc/{provider}/start
// and /auth/oidc/{provider}/callback paths.
func oidcProviderSegment(r *http.Request) string {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 4 && parts[0] == "auth" && parts[1] == "oidc" {
		return parts[2]
	}
	return ""
}
