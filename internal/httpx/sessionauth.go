// sessionauth.go — browser login handlers on server-side revocable sessions
// (AUTH-03): local credential login, logout with server-side revocation and
// the OIDC start/callback/link endpoints. Constructed in main and mounted on
// the mux by the dashboard wiring; every path that mints a session runs a
// verified credential first — no path issues a session for an unverified
// identity.
package httpx

import (
	"crypto/sha256"
	"fmt"
	"html/template"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
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
	auth           *auth.Service
	oidc           *auth.OIDCService
	sessionTTL     time.Duration
	templates      *template.Template
	tenantSlug     string
	securityAudit  SecurityAuditRecorder
	loginIPLimiter *RateLimiter
	loginAcctLimit *RateLimiter
	loginAcctsMu   sync.Mutex
	loginAccts     map[string]time.Time
	trustProxy     bool
}

// NewSessionAuth builds the handler set. oidc may be nil — provider links
// then never render and the OIDC handlers deny. tenantSlug identifies the
// tenant the browser login targets.
func NewSessionAuth(authSvc *auth.Service, oidc *auth.OIDCService, sessionTTL time.Duration, templates *template.Template, tenantSlug string) *SessionAuth {
	return &SessionAuth{
		auth:           authSvc,
		oidc:           oidc,
		sessionTTL:     sessionTTL,
		templates:      templates,
		tenantSlug:     strings.TrimSpace(tenantSlug),
		loginIPLimiter: NewRateLimiter(),
		loginAcctLimit: NewRateLimiter(),
		loginAccts:     make(map[string]time.Time),
	}
}

func (s *SessionAuth) SetTrustProxy(trust bool) {
	s.trustProxy = trust
}

// loginAccountCap bounds distinct tenant+email buckets so a spray of
// throwaway addresses cannot grow the limiter map without limit.
const loginAccountCap = 4096

const loginAccountWindow = 15 * time.Minute

// Login throttling is per process; use shared admission storage for multi-instance enforcement.
// allowPasswordAttempt gates the credential check WITHOUT consuming budget:
// only failures count (recordPasswordFailure), so successful sign-ins never
// erode either limit.
func (s *SessionAuth) allowPasswordAttempt(w http.ResponseWriter, r *http.Request, tenantID, email string) bool {
	now := time.Now()
	ip := clientIP(r, s.trustProxy)
	if dec := s.loginIPLimiter.CheckKey(ip, 10, time.Minute, now); !dec.Allowed {
		return s.denyPasswordAttempt(w, r, tenantID, email, dec.RetryAfter)
	}
	key := loginAccountKey(tenantID, email)
	if dec := s.loginAcctLimit.CheckKey(key, 5, loginAccountWindow, now); !dec.Allowed {
		return s.denyPasswordAttempt(w, r, tenantID, email, dec.RetryAfter)
	}
	return true
}

// recordPasswordFailure counts one failed credential check against the
// caller IP and the normalized tenant+email bucket.
func (s *SessionAuth) recordPasswordFailure(r *http.Request, tenantID, email string) {
	now := time.Now()
	s.loginIPLimiter.AllowKey(clientIP(r, s.trustProxy), 10, time.Minute, now)
	key := loginAccountKey(tenantID, email)
	// The account map bounds distinct buckets; a full map of live entries
	// skips account limiting while the IP limit still applies.
	if s.trackLoginAccount(key, now) {
		s.loginAcctLimit.AllowKey(key, 5, loginAccountWindow, now)
	}
}

// loginAccountKey hashes tenant+normalized email so the limiter never holds
// the raw address.
func loginAccountKey(tenantID, email string) string {
	account := sha256.Sum256([]byte(tenantID + "\x00" + strings.ToLower(strings.TrimSpace(email))))
	return fmt.Sprintf("%x", account)
}

// trackLoginAccount reports whether the account key is (or may be) tracked.
// At capacity, expired buckets are evicted first; account limiting is skipped
// only while the cap is genuinely full of live entries — the IP limit applies.
func (s *SessionAuth) trackLoginAccount(key string, now time.Time) bool {
	s.loginAcctsMu.Lock()
	defer s.loginAcctsMu.Unlock()
	if _, ok := s.loginAccts[key]; ok {
		s.loginAccts[key] = now
		return true
	}
	if len(s.loginAccts) >= loginAccountCap {
		for k, seen := range s.loginAccts {
			if now.Sub(seen) >= loginAccountWindow {
				delete(s.loginAccts, k)
			}
		}
		if len(s.loginAccts) >= loginAccountCap {
			return false
		}
	}
	s.loginAccts[key] = now
	return true
}

func (s *SessionAuth) denyPasswordAttempt(w http.ResponseWriter, r *http.Request, tenantID, email string, retryAfterSecs int64) bool {
	s.recordSecurityAudit(r, auth.Principal{TenantID: tenantID}, "auth.login.throttled", "session", "", "throttled")
	setNoStoreHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusTooManyRequests)
	mins := (retryAfterSecs + 59) / 60
	if mins < 1 {
		mins = 1
	}
	s.renderLogin(w, r, dashboardLoginData{Email: email, ReturnTo: r.FormValue("return_to"), Error: fmt.Sprintf("Too many sign-in attempts. Try again in %d minutes.", mins)})
	return false
}

func (s *SessionAuth) tenantID(r *http.Request) (string, error) {
	return s.auth.ResolveTenantID(r.Context(), s.tenantSlug)
}

func (s *SessionAuth) renderLogin(w http.ResponseWriter, r *http.Request, data dashboardLoginData) {
	setNoStoreHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	view := dashboardLoginView{
		dashboardLoginData: data,
		Org:                s.orgName(r.Context()),
		Theme:              themeClass(r),
	}
	if err := s.templates.ExecuteTemplate(w, "login.tmpl", view); err != nil {
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
	if !s.allowPasswordAttempt(w, r, tenantID, email) {
		return
	}
	principal, err := s.auth.AuthenticatePassword(r.Context(), tenantID, email, r.FormValue("password"))
	if err != nil {
		s.recordPasswordFailure(r, tenantID, email)
		s.recordSecurityAudit(r, auth.Principal{TenantID: tenantID}, "auth.login.failed", "session", "", "failure")
		s.renderLogin(w, r, dashboardLoginData{
			Email: email, ReturnTo: returnTo,
			Error: "Invalid email or password",
		})
		return
	}
	if !s.issueSessionCookies(w, r, principal.TenantID, principal.UserID) {
		return
	}
	target := "/dashboard"
	if returnTo != "" {
		target = returnTo
	}
	http.Redirect(w, r, target, http.StatusSeeOther) //nolint:gosec // safeLocalRedirect rejects non-local return targets above.
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
	if !sameOriginMutation(r, s.trustProxy) ||
		s.auth.ValidateCSRF(r.Context(), principal.TenantID, principal.SessionID,
			csrfFromRequest(r)) != nil {
		WriteForbidden(w, "invalid CSRF token")
		return
	}
	if err := s.auth.RevokeSession(r.Context(), principal.TenantID, principal.SessionID, "logout"); err != nil {
		log.Printf("revoke session %s: %v", principal.SessionID, err)
		s.recordSecurityAudit(r, principal, "auth.logout", "session", principal.SessionID, "failure")
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "session_revoke_failed")
		return
	} else {
		s.recordSecurityAudit(r, principal, "auth.logout", "session", principal.SessionID, "success")
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
		s.recordSecurityAudit(r, auth.Principal{TenantID: tenantID}, "auth.login.failed", "session", "", "failure")
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
	if !s.issueSessionCookies(w, r, tenantID, res.UserID) {
		return
	}
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
	if !s.allowPasswordAttempt(w, r, tenantID, email) {
		return
	}
	res, err := s.oidc.ConfirmLink(r.Context(), tenantID, linkToken, email, r.FormValue("password"))
	if err != nil {
		s.recordSecurityAudit(r, auth.Principal{TenantID: tenantID}, "auth.login.failed", "session", "", "failure")
		s.renderLogin(w, r, dashboardLoginData{
			LinkToken: linkToken, LinkEmail: email, ReturnTo: returnTo,
			Error: "Account linking failed — check the account credentials",
		})
		return
	}
	if !s.issueSessionCookies(w, r, tenantID, res.UserID) {
		return
	}
	target := "/dashboard"
	if res.ReturnTo != "" {
		target = res.ReturnTo
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// SessionPrincipal resolves the request's session cookie to a verified
// principal — the tenant comes only from the verified session row.
func (s *SessionAuth) SessionPrincipal(r *http.Request) (auth.Principal, bool) {
	if s == nil || s.auth == nil {
		return auth.Principal{}, false
	}
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
// redirects to login; API/HTMX requests deny with 401. Cookie-authenticated
// mutations additionally require the session's CSRF token. The principal is
// placed on the request context for downstream handlers. A nil SessionAuth
// denies — the dashboard cannot run without constructed session handlers.
func (s *SessionAuth) RequireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := s.SessionPrincipal(r)
		if !ok {
			// Expired/absent session on an htmx request: 401 + HX-Redirect to
			// a FULL page (htmx follows the header; a 303 or fragment body
			// would be swapped into the page). The return target comes from
			// HX-Current-URL, never the fragment's own RequestURI.
			if r.Header.Get("HX-Request") != "" {
				w.Header().Set("HX-Redirect", "/dashboard/login?return_to="+url.QueryEscape(hxLoginReturnTo(r)))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.Method == http.MethodGet {
				if target := r.URL.RequestURI(); safeLocalRedirect(target) && target != "" {
					http.Redirect(w, r, "/dashboard/login?return_to="+url.QueryEscape(target), http.StatusSeeOther)
					return
				}
				http.Redirect(w, r, "/dashboard/login", http.StatusSeeOther)
				return
			}
			WriteUnauthorized(w, "dashboard login required")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if !sameOriginMutation(r, s.trustProxy) ||
				s.auth.ValidateCSRF(r.Context(), principal.TenantID, principal.SessionID,
					csrfFromRequest(r)) != nil {
				WriteForbidden(w, "missing or invalid CSRF token")
				return
			}
		}
		next(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	}
}

// RequireSessionScope composes RequireSession with the scope check the API
// applies through authenticated(scope, ...): the session principal must hold
// scope before the handler runs. Dashboard routes use it to mirror the
// permission of the equivalent /api/v1 endpoint — a signed-in member without
// the scope gets a truthful 403, never tenant-wide data or admin mutations.
func (s *SessionAuth) RequireSessionScope(scope string, next http.HandlerFunc) http.HandlerFunc {
	return s.RequireSession(requireSessionScope(scope, next))
}

// requireSessionScope denies a request whose verified principal lacks scope.
// It runs inside RequireSession, so the principal is always present; the
// fail-closed branch still covers a context built without one.
func requireSessionScope(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFromContext(r.Context())
		if !ok || !auth.HasScope(p, scope) {
			WriteForbidden(w, "missing required scope: "+scope)
			return
		}
		next(w, r)
	}
}

// issueSessionCookies mints the server-side session and sets the opaque
// HttpOnly cookie plus a script-readable CSRF cookie — never a signed
// client-side value.
func (s *SessionAuth) issueSessionCookies(w http.ResponseWriter, r *http.Request, tenantID, userID string) bool {
	token, csrf, sessionID, err := s.auth.IssueSession(r.Context(), tenantID, userID, s.sessionTTL)
	if err != nil {
		log.Printf("issue session for %s: %v", userID, err)
		s.recordSecurityAudit(r, auth.Principal{TenantID: tenantID, UserID: userID}, "auth.login.failed", "session", "", "failure")
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "session_issue_failed")
		return false
	}
	secure := strings.HasPrefix(requestOrigin(r, s.trustProxy), "https://")
	expires := time.Now().Add(s.sessionTTL)
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure follows the external scheme to support HTTP development.
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
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // CSRF cookie must be script-readable; Secure follows the external scheme.
		Name:     sessionCSRFCookieName,
		Value:    csrf,
		Path:     "/",
		Expires:  expires,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
	s.recordSecurityAudit(r, auth.Principal{TenantID: tenantID, UserID: userID, ActorID: userID, ActorKind: auth.ActorKindUser, CredentialID: sessionID, CredentialKind: auth.CredentialKindSession, SessionID: sessionID}, "auth.login.succeeded", "session", sessionID, "success")
	return true
}

func (s *SessionAuth) clearSessionCookies(w http.ResponseWriter, r *http.Request) {
	secure := strings.HasPrefix(requestOrigin(r, s.trustProxy), "https://")
	for _, name := range []string{sessionCookieName, sessionCSRFCookieName} {
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // Expired cookies match the attributes used on issuance.
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

// csrfFromRequest reads the CSRF token the mutation actually supplied: the
// X-CSRF-Token header, or — for real HTML form posts — the csrf_token field.
// The enclava_csrf cookie is NEVER a source: it is sent automatically, so
// treating it as the token would let any cross-site request authenticate a
// mutation (AUTH-I01). PostFormValue caches the urlencoded body for the
// handler that re-parses it.
func csrfFromRequest(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-CSRF-Token")); v != "" {
		return v
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return ""
	}
	if mediaType == "application/x-www-form-urlencoded" || mediaType == "multipart/form-data" {
		return strings.TrimSpace(r.PostFormValue("csrf_token"))
	}
	return ""
}

// csrfCookieValue reads the script-readable CSRF cookie for server-side form
// rendering (the logout form's hidden csrf_token input).
func csrfCookieValue(r *http.Request) string {
	if cookie, err := r.Cookie(sessionCSRFCookieName); err == nil {
		return cookie.Value
	}
	return ""
}

// sameOriginMutation enforces the fetch-metadata contract on cookie
// authentication: Sec-Fetch-Site (when the browser sends it) must be
// same-origin or none; when it is absent an Origin header must name this
// deployment's own origin. Both absent → the CSRF token is the only proof.
// Required because the script-readable CSRF cookie is shared with sibling
// ports/subdomains, so the token alone does not stop them (AUTH-I01).
func sameOriginMutation(r *http.Request, trustProxy bool) bool {
	if site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); site != "" {
		return site == "same-origin" || site == "none"
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		return strings.EqualFold(origin, requestOrigin(r, trustProxy))
	}
	return true
}

// requestOrigin is the origin the browser addresses (the externalBase
// scheme://host shape), honoring forwarded proto/host only when the
// deployment trusts proxy headers.
func requestOrigin(r *http.Request, trustProxy bool) string {
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	host := r.Host
	if trustProxy {
		if v := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); v == "http" || v == "https" {
			scheme = v
		}
		if v := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); v != "" {
			host = v
		}
	}
	return scheme + "://" + host
}

// returnToParam reads + allowlist-checks the ?return_to query param.
func returnToParam(r *http.Request) string {
	target := r.URL.Query().Get("return_to")
	if safeLocalRedirect(target) {
		return target
	}
	return ""
}

// hxLoginReturnTo selects the FULL-page return target for an expired htmx
// request: the same-origin page identified by HX-Current-URL (absolute URL
// on this host, or a root-absolute path), else /dashboard. Fragment,
// partial and login targets are rejected so login never loops and a swap
// never renders the expired fragment's URL.
func hxLoginReturnTo(r *http.Request) string {
	u, err := url.Parse(strings.TrimSpace(r.Header.Get("HX-Current-URL")))
	if err != nil || (u.Host != "" && u.Host != r.Host) {
		return "/dashboard"
	}
	target := u.RequestURI()
	if !safeLocalRedirect(target) || u.Path == "" ||
		strings.HasPrefix(u.Path, "/dashboard/partials/") || u.Path == "/dashboard/login" {
		return "/dashboard"
	}
	return target
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
