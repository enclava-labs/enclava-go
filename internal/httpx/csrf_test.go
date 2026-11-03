// csrf_test.go — AUTH-I01 regression coverage: the CSRF token must come from
// X-CSRF-Token or a csrf_token form field (never the automatically-sent
// cookie), and cookie-authenticated mutations must be same-origin by fetch
// metadata. Exercises the real mux over the migrated schema.
package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"enclava-go/internal/auth"
)

// sessionReq builds one cookie-authenticated mutation through the real mux.
// token=="" sends no header/field; the CSRF cookie still rides like a real
// browser would send it. headers set extra request headers (Sec-Fetch-Site,
// Origin).
func sessionReq(t *testing.T, app *App, method, path string, form url.Values, session, csrf *http.Cookie, token string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.AddCookie(session)
	if csrf != nil {
		r.AddCookie(csrf)
	}
	if token != "" {
		r.Header.Set("X-CSRF-Token", token)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, r)
	return rec
}

func TestCookieAuthMutationRequiresSuppliedCSRFToken(t *testing.T) {
	f := newIdentityFixture(t)
	all := auth.ResourceGrants{
		Models:           auth.Allowlist{All: true},
		Endpoints:        auth.Allowlist{All: true},
		Assistants:       auth.Allowlist{All: true},
		ExtractTemplates: auth.Allowlist{All: true},
	}
	sess, csrf := loginUserWithGrants(t, f, "csrf-admin@example.com", []string{auth.ScopePlatformAdmin}, all, "csrf-pass")

	create := func(name, token string, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/admin/api-keys",
			strings.NewReader(`{"name":"`+name+`","scopes":["platform.admin"]}`))
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(sess)
		r.AddCookie(csrf)
		if token != "" {
			r.Header.Set("X-CSRF-Token", token)
		}
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		f.app.Handler().ServeHTTP(rec, r)
		return rec
	}

	// The cookie alone never authenticates a mutation (the AUTH-I01 hole).
	if rec := create("csrf-denied", "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("cookie-only POST: expected 403, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Supplied token + same-origin fetch metadata: allowed.
	if rec := create("csrf-same-origin", csrf.Value, map[string]string{"Sec-Fetch-Site": "same-origin"}); rec.Code != http.StatusCreated {
		t.Fatalf("same-origin POST: expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Supplied token but cross-origin metadata: sibling ports/subdomains
	// share the readable cookie, so both are rejected.
	for _, site := range []string{"same-site", "cross-site"} {
		if rec := create("csrf-"+site, csrf.Value, map[string]string{"Sec-Fetch-Site": site}); rec.Code != http.StatusForbidden {
			t.Fatalf("Sec-Fetch-Site %s: expected 403, got %d", site, rec.Code)
		}
	}

	// No fetch metadata: Origin must equal the app's own origin — a sibling
	// port is a different origin and must be rejected.
	if rec := create("csrf-sibling", csrf.Value, map[string]string{"Origin": "http://example.com:8080"}); rec.Code != http.StatusForbidden {
		t.Fatalf("sibling-port Origin: expected 403, got %d", rec.Code)
	}
	if rec := create("csrf-own-origin", csrf.Value, map[string]string{"Origin": "http://example.com"}); rec.Code != http.StatusCreated {
		t.Fatalf("own Origin: expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestCookieAuthFormFieldCSRF proves the HTML-form path: the logout form's
// hidden csrf_token field (no X-CSRF-Token header) satisfies the check.
func TestCookieAuthFormFieldCSRF(t *testing.T) {
	f := newIdentityFixture(t)
	sess, csrf := loginUser(t, f, "csrf-form@example.com", []string{"chatbot"}, "csrf-pass")

	// No token at all — cookie only — denied.
	if rec := sessionReq(t, f.app, http.MethodPost, "/dashboard/logout",
		url.Values{}, sess, csrf, "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("cookie-only logout: expected 403, got %d", rec.Code)
	}

	// The form field alone (what the rendered logout form submits) works.
	rec := sessionReq(t, f.app, http.MethodPost, "/dashboard/logout",
		url.Values{"csrf_token": {csrf.Value}}, sess, csrf, "",
		map[string]string{"Sec-Fetch-Site": "same-origin"})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/dashboard/login" {
		t.Fatalf("form-field logout: expected 303 to login, got %d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
}

// TestBearerAPIKeySkipsCSRF proves API-key mutations stay exempt: no CSRF
// token, no fetch metadata needed.
func TestBearerAPIKeySkipsCSRF(t *testing.T) {
	f := newIdentityFixture(t)
	rec := f.admin(t, http.MethodPost, "/api/v1/admin/api-keys",
		`{"name":"bearer-no-csrf","scopes":["platform.admin"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("API-key POST without CSRF: expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestCSRFTokenSources pins the extractor contract directly: header or form
// field only, never the cookie.
func TestCSRFTokenSources(t *testing.T) {
	mk := func(contentType, body string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		r.Header.Set("Cookie", sessionCSRFCookieName+"=cookie-token")
		return r
	}
	if got := csrfFromRequest(mk("application/json", `{}`)); got != "" {
		t.Fatalf("cookie fell back as the token: %q", got)
	}
	r := mk("application/json", `{}`)
	r.Header.Set("X-CSRF-Token", " header-token ")
	if got := csrfFromRequest(r); got != "header-token" {
		t.Fatalf("header token = %q", got)
	}
	if got := csrfFromRequest(mk("application/x-www-form-urlencoded", "csrf_token=field-token&x=1")); got != "field-token" {
		t.Fatalf("form token = %q", got)
	}
	if got := csrfFromRequest(mk("text/plain", "csrf_token=field-token")); got != "" {
		t.Fatalf("non-form content type must not read the body: %q", got)
	}
}

// TestSameOriginMutation pins the fetch-metadata rules without the mux.
func TestSameOriginMutation(t *testing.T) {
	mk := func() *http.Request { return httptest.NewRequest(http.MethodPost, "http://app.internal/x", nil) }
	for _, tc := range []struct {
		name   string
		site   string
		origin string
		want   bool
	}{
		{"same-origin", "same-origin", "", true},
		{"none", "none", "", true},
		{"same-site", "same-site", "", false},
		{"cross-site", "cross-site", "", false},
		{"no metadata", "", "", true},
		{"own origin", "", "http://app.internal", true},
		{"sibling port", "", "http://app.internal:9000", false},
		{"other host", "", "https://evil.example", false},
		{"null origin", "", "null", false},
	} {
		r := mk()
		if tc.site != "" {
			r.Header.Set("Sec-Fetch-Site", tc.site)
		}
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if got := sameOriginMutation(r, false); got != tc.want {
			t.Errorf("%s: sameOriginMutation = %v, want %v", tc.name, got, tc.want)
		}
	}

	// TRUST_PROXY_HEADERS on: the own origin follows the forwarded proto/host.
	r := mk()
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "admin.example.com")
	r.Header.Set("Origin", "https://admin.example.com")
	if !sameOriginMutation(r, true) {
		t.Error("trusted forwarded origin must count as same-origin")
	}
	if sameOriginMutation(r, false) {
		t.Error("untrusted forwarded headers must not change the own origin")
	}
}
