package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
		req := httptest.NewRequest(http.MethodGet, "/dashboard/partials/stats", nil)
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
