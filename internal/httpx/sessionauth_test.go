package httpx

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/store/postgres"
)

// postLogin submits the local credential form through the real mux and
// returns the recorder plus the issued session and CSRF cookies.
func postLogin(t *testing.T, app *App, email, password, returnTo string) (*httptest.ResponseRecorder, *http.Cookie, *http.Cookie) {
	t.Helper()
	form := url.Values{"email": {email}, "password": {password}}
	if returnTo != "" {
		form.Set("return_to", returnTo)
	}
	req := httptest.NewRequest(http.MethodPost, "/dashboard/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login expected 303, got %d body=%s", rec.Code, rec.Body.String())
	}
	return rec, sessionCookie(t, rec, sessionCookieName), sessionCookie(t, rec, sessionCSRFCookieName)
}

// TestSessionRedirectPreservesReturnTo pins the deep-link contract: an
// unauthenticated page GET carries the full local path+query through login
// and lands on the originally requested URL.
func TestSessionRedirectPreservesReturnTo(t *testing.T) {
	app, repo, tenant := newSessionApp(t)
	seedLoginUser(t, repo, tenant, "login@example.com", "correct-horse")

	const deepLink = "/dashboard/chat/conv_42?x=1"
	req := httptest.NewRequest(http.MethodGet, deepLink, nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauth GET expected 303, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Location"), "/dashboard/login?return_to="+url.QueryEscape(deepLink); got != want {
		t.Fatalf("expected login redirect preserving path+query, got %q want %q", got, want)
	}

	// The login form's return_to lands the user on the deep link.
	rec, _, _ = postLogin(t, app, "login@example.com", "correct-horse", deepLink)
	if got := rec.Header().Get("Location"); got != deepLink {
		t.Fatalf("login with return_to expected redirect to %q, got %q", deepLink, got)
	}

	// A non-local request target falls back to the bare login redirect. The
	// request goes straight to the session guard: ServeMux canonicalizes
	// slashless paths with a 307 before RequireSession ever runs.
	var hit bool
	guarded := app.sessionAuth.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	unsafeReq := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	unsafeReq.URL.Path = "evil.com/x"
	unsafeReq.RequestURI = "evil.com/x"
	unsafeRec := httptest.NewRecorder()
	guarded.ServeHTTP(unsafeRec, unsafeReq)
	if hit || unsafeRec.Code != http.StatusSeeOther || unsafeRec.Header().Get("Location") != "/dashboard/login" {
		t.Fatalf("unsafe request target expected bare /dashboard/login redirect, got %d %q (handler reached: %v)",
			unsafeRec.Code, unsafeRec.Header().Get("Location"), hit)
	}
}

// TestSessionHXExpiryRedirects pins the B12 htmx-expiry contract: 401 with
// an empty body and HX-Redirect to login whose return_to is the validated
// same-origin FULL page from HX-Current-URL — never the fragment URL — and
// the login chain lands on that full page.
func TestSessionHXExpiryRedirects(t *testing.T) {
	app, repo, tenant := newSessionApp(t)
	seedLoginUser(t, repo, tenant, "login@example.com", "correct-horse")

	_, session, _ := postLogin(t, app, "login@example.com", "correct-horse", "")

	// Expire the session server-side so the deny path is the real revocation
	// path, not a missing cookie.
	authReq := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	authReq.AddCookie(session)
	principal, ok := app.sessionAuth.SessionPrincipal(authReq)
	if !ok {
		t.Fatalf("fresh session should authenticate")
	}
	if err := app.sessionAuth.auth.RevokeSession(context.Background(),
		principal.TenantID, principal.SessionID, "test-expiry"); err != nil {
		t.Fatalf("revoke session: %v", err)
	}

	hxRequest := func(currentURL string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/dashboard/partials/home/running", nil)
		req.Header.Set("HX-Request", "true")
		if currentURL != "" {
			req.Header.Set("HX-Current-URL", currentURL)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}

	// Revoked-session htmx request: 401, empty body, redirect to the full page.
	rec := hxRequest("http://example.com/dashboard/knowledge", session)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired htmx request expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("expired htmx request must return an empty body, got %q", rec.Body.String())
	}
	if got, want := rec.Header().Get("HX-Redirect"), "/dashboard/login?return_to=%2Fdashboard%2Fknowledge"; got != want {
		t.Fatalf("expected HX-Redirect to full page, got %q want %q", got, want)
	}

	// Replaying login with that return_to lands on the full page.
	rec, _, _ = postLogin(t, app, "login@example.com", "correct-horse", "/dashboard/knowledge")
	if got := rec.Header().Get("Location"); got != "/dashboard/knowledge" {
		t.Fatalf("login after htmx expiry expected landing on full page, got %q", got)
	}

	for _, tc := range []struct {
		name    string
		current string
		want    string
	}{
		{"fragment current-url falls back", "http://example.com/dashboard/partials/knowledge/documents", "/dashboard"},
		{"external origin falls back", "https://evil.example/x", "/dashboard"},
		{"login target falls back", "http://example.com/dashboard/login", "/dashboard"},
		{"absent header falls back", "", "/dashboard"},
		{"root-absolute same-origin page honored", "/dashboard/knowledge?tab=2", "/dashboard/knowledge?tab=2"},
	} {
		rec := hxRequest(tc.current, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: expected 401, got %d body=%s", tc.name, rec.Code, rec.Body.String())
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("%s: expected empty body, got %q", tc.name, rec.Body.String())
		}
		if got, want := rec.Header().Get("HX-Redirect"), "/dashboard/login?return_to="+url.QueryEscape(tc.want); got != want {
			t.Fatalf("%s: expected HX-Redirect %q, got %q", tc.name, want, got)
		}
	}
}

func TestPasswordLoginRateLimits(t *testing.T) {
	s := newTestSessionAuth()
	attempt := func(ip, tenant, email string) bool {
		r := httptest.NewRequest(http.MethodPost, "/dashboard/login", nil)
		r.RemoteAddr = ip + ":1234"
		r.Header.Set("X-Forwarded-For", "203.0.113.1")
		rec := httptest.NewRecorder()
		allowed := s.allowPasswordAttempt(rec, r, tenant, email)
		if !allowed && (rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "Too many sign-in attempts. Try again later.")) {
			t.Fatalf("unexpected throttled response: %d %s", rec.Code, rec.Body.String())
		}
		return allowed
	}
	for i := range 5 {
		if !attempt(fmt.Sprintf("192.0.2.%d", i+1), "tenant", "User@Example.com") {
			t.Fatal("account limited too early")
		}
	}
	if attempt("192.0.2.6", "tenant", " user@example.com ") {
		t.Fatal("account limit must apply across IPs and normalized email")
	}
	for key, window := range s.loginAcctLimit.windows {
		window.start = window.start.Add(-2 * time.Minute)
		s.loginAcctLimit.windows[key] = window
	}
	if attempt("192.0.2.7", "tenant", "user@example.com") {
		t.Fatal("account limit must last 15 minutes")
	}
	if !attempt("192.0.2.6", "other-tenant", "user@example.com") {
		t.Fatal("account limit must be tenant scoped")
	}
	for i := range 10 {
		if !attempt("198.51.100.1", "tenant", fmt.Sprintf("user%d@example.com", i)) {
			t.Fatal("IP limited too early")
		}
	}
	if attempt("198.51.100.1", "tenant", "another@example.com") {
		t.Fatal("IP limit must apply across accounts")
	}
}

func newTestSessionAuth() *SessionAuth {
	return &SessionAuth{
		loginIPLimiter: NewRateLimiter(),
		loginAcctLimit: NewRateLimiter(),
		loginAccts:     make(map[string]time.Time),
		templates:      template.Must(template.New("login.tmpl").Parse("{{.Error}}")),
	}
}

func TestThrottledIPCreatesNoAccountEntries(t *testing.T) {
	s := newTestSessionAuth()
	for i := range 10 {
		r := httptest.NewRequest(http.MethodPost, "/dashboard/login", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		s.allowPasswordAttempt(httptest.NewRecorder(), r, "tenant", fmt.Sprintf("u%d@example.com", i))
	}
	if len(s.loginAccts) != 10 {
		t.Fatalf("pre-throttle accounts = %d, want 10", len(s.loginAccts))
	}
	for i := 10; i < 60; i++ {
		r := httptest.NewRequest(http.MethodPost, "/dashboard/login", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		s.allowPasswordAttempt(httptest.NewRecorder(), r, "tenant", fmt.Sprintf("u%d@example.com", i))
	}
	if len(s.loginAccts) != 10 || len(s.loginAcctLimit.windows) != 10 {
		t.Fatalf("throttled IP grew account state: %d buckets, %d windows",
			len(s.loginAccts), len(s.loginAcctLimit.windows))
	}
}

func TestLoginAccountCapSkipsNewAccounts(t *testing.T) {
	s := newTestSessionAuth()
	now := time.Now()
	if !s.trackLoginAccount("k", now) {
		t.Fatal("empty limiter must accept a new account")
	}
	for len(s.loginAccts) < loginAccountCap {
		s.trackLoginAccount(fmt.Sprintf("acct-%d", len(s.loginAccts)), now)
	}
	if !s.trackLoginAccount("k", now) {
		t.Fatal("tracked account must keep being limited")
	}
	if s.trackLoginAccount("new", now) {
		t.Fatal("new account beyond cap must be skipped")
	}
	r := httptest.NewRequest(http.MethodPost, "/dashboard/login", nil)
	r.RemoteAddr = "198.51.100.2:1234"
	for range 10 {
		if !s.allowPasswordAttempt(httptest.NewRecorder(), r, "tenant", "new-account@example.com") {
			t.Fatal("capped account must not deny before the IP limit")
		}
	}
	if s.allowPasswordAttempt(httptest.NewRecorder(), r, "tenant", "new-account@example.com") {
		t.Fatal("IP limit must still deny when account limiting is skipped")
	}
}

func TestLoginAccountCapEvictsExpired(t *testing.T) {
	s := newTestSessionAuth()
	old := time.Now().Add(-loginAccountWindow - time.Minute)
	for len(s.loginAccts) < loginAccountCap {
		s.trackLoginAccount(fmt.Sprintf("acct-%d", len(s.loginAccts)), old)
	}
	// An account bucket older than its window is evicted at capacity.
	if !s.trackLoginAccount("fresh", time.Now()) {
		t.Fatal("expired buckets must free capacity for a new account")
	}
	if len(s.loginAccts) != 1 {
		t.Fatalf("expired buckets retained: %d", len(s.loginAccts))
	}
}

type loginAuditRecorder struct {
	rows []postgres.SecurityAuditRow
}

func (a *loginAuditRecorder) Insert(_ context.Context, row postgres.SecurityAuditRow) error {
	a.rows = append(a.rows, row)
	return nil
}

func (*loginAuditRecorder) List(context.Context, string, int, string) ([]postgres.SecurityAuditRow, error) {
	return nil, nil
}

func TestThrottledLoginIsAuditedWithoutEmail(t *testing.T) {
	audit := &loginAuditRecorder{}
	s := newTestSessionAuth()
	s.securityAudit = audit
	for range 6 {
		r := httptest.NewRequest(http.MethodPost, "/dashboard/login", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		s.allowPasswordAttempt(httptest.NewRecorder(), r, "tenant", "secret@example.com")
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "auth.login.throttled" || audit.rows[0].Outcome != "throttled" || audit.rows[0].TenantID != "tenant" || audit.rows[0].TargetID != "" {
		t.Fatalf("unexpected audit records: %+v", audit.rows)
	}
}
