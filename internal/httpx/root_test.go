package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestRootRedirectsToDashboard(t *testing.T) {
	app := NewApp(nil, nil, "", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, "", "", "", nil, nil, 1, false, nil, nil, GateConfig{}, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected status 303, got %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/dashboard" {
		t.Fatalf("expected redirect to /dashboard, got %q", got)
	}
}

func TestRootCatchAllStillReturnsNotFound(t *testing.T) {
	app := NewApp(nil, nil, "", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, "", "", "", nil, nil, 1, false, nil, nil, GateConfig{}, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", rec.Code)
	}
}

// seedLoginUser inserts an active user with a known plaintext password so the
// session login flow can authenticate against the real identity store.
func seedLoginUser(t *testing.T, repo *postgres.IdentityRepo, tenantID, email, password string) {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	now := time.Now().UTC()
	user := auth.User{
		ID:            "user_login_test",
		TenantID:      tenantID,
		Email:         email,
		DisplayName:   email,
		Status:        "active",
		PasswordHash:  hash,
		PasswordSetAt: &now,
	}
	if err := repo.CreateUser(context.Background(), tenantID, user); err != nil {
		t.Fatalf("seed login user: %v", err)
	}
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("expected %s cookie in response", name)
	return nil
}

func newSessionApp(t *testing.T) (*App, *postgres.IdentityRepo, string) {
	t.Helper()
	db := testdb.New(t)
	repo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "bootstrap")
	authSvc := auth.NewService(repo, "")
	sessionAuth := NewSessionAuth(authSvc, nil, time.Hour, dashboardTemplates, "bootstrap")
	app := NewApp(authSvc, nil, "", nil, repo, nil, nil, nil, nil, nil, nil, nil, nil, "sha256", "", "enc_", nil, sessionAuth, 1, false, nil, nil, GateConfig{}, nil, nil)
	return app, repo, tenant
}

func TestDashboardSessionLoginFlow(t *testing.T) {
	app, repo, tenant := newSessionApp(t)
	seedLoginUser(t, repo, tenant, "login@example.com", "correct-horse")

	// Unauthenticated GET redirects to login.
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/dashboard/login" {
		t.Fatalf("expected 303 to /dashboard/login, got %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// Login page renders; no OIDC providers configured → none listed.
	req = httptest.NewRequest(http.MethodGet, "/dashboard/login", nil)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login page expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "/auth/oidc/") {
		t.Fatalf("login page rendered OIDC links with no providers enabled")
	}

	// Wrong password → inline error, no session cookie. A user-less email must
	// render the same error page without panicking on the template data map.
	for _, email := range []string{"login@example.com", "ghost@example.com"} {
		form := url.Values{"email": {email}, "password": {"wrong"}}
		req = httptest.NewRequest(http.MethodPost, "/dashboard/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec = httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("bad login expected 200 error page, got %d body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Invalid email or password") {
			t.Fatalf("expected inline error for %s, got body=%s", email, rec.Body.String())
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == sessionCookieName {
				t.Fatalf("bad login for %s must not issue a session cookie", email)
			}
		}
	}

	// Correct credentials → session + CSRF cookies, redirect to dashboard.
	form := url.Values{"email": {"login@example.com"}, "password": {"correct-horse"}}
	req = httptest.NewRequest(http.MethodPost, "/dashboard/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/dashboard" {
		t.Fatalf("login expected 303 to /dashboard, got %d %q body=%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	session := sessionCookie(t, rec, sessionCookieName)
	csrf := sessionCookie(t, rec, sessionCSRFCookieName)

	// Session cookie authorizes the page.
	req = httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(session)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard GET with session expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Mutation without the CSRF header is rejected even with a valid session.
	mut := url.Values{"action": {"create"}, "name": {"k1"}}
	req = httptest.NewRequest(http.MethodPost, "/dashboard/partials/keys", strings.NewReader(mut.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(session)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("mutation without CSRF expected 403, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Mutation with the session's CSRF token succeeds.
	req = httptest.NewRequest(http.MethodPost, "/dashboard/partials/keys", strings.NewReader(mut.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(session)
	req.AddCookie(csrf)
	req.Header.Set("X-CSRF-Token", csrf.Value)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mutation with CSRF expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Logout revokes the session server-side; the next GET is denied.
	req = httptest.NewRequest(http.MethodPost, "/dashboard/logout", nil)
	req.AddCookie(session)
	req.AddCookie(csrf)
	req.Header.Set("X-CSRF-Token", csrf.Value)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("logout expected 303, got %d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(session)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/dashboard/login" {
		t.Fatalf("post-logout GET expected 303 to login, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
}
