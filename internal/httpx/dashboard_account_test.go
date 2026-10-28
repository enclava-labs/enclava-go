// dashboard_account_test.go — Account page (OA-30, plan 09-09) render tests:
// the one session-only page every role reaches, with strictly self-service
// content. Every test runs against the real migrated schema via the identity
// fixture; page renders go through the real mux with real sessions.
//
// Obsolete-surface handoff for 09-10 (shared with 09-07's cutover): once BOTH
// the Access page (09-07) and this Account page are landed, 09-10 deletes the
// legacy self-service surface — `handleDashboardPageIdentity` in dashboard.go
// (whole func — its admin half is replaced by 09-07, its self-service half by
// this plan), the `/dashboard/identity` registration line in
// `registerLegacyDashboardRoutes`, and `dashboard/templates/page_identity.tmpl`.
package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/testdb"
)

// accountLogin seeds a user with a known password, logs in through the real
// browser route and returns the session + CSRF cookies.
func accountLogin(t *testing.T, f *identityFixture, email, password string, scopes []string) auth.User {
	t.Helper()
	user := testdb.SeedUser(t, f.db, f.tenant, email, scopes)
	setPassword(t, f.repo, f.tenant, user.ID, password)
	return user
}

func TestDashboardAccountRenders(t *testing.T) {
	f := newIdentityFixture(t)
	for _, tc := range []struct {
		name     string
		email    string
		password string
		scopes   []string
	}{
		{"admin", "acct-admin@example.com", "acct-admin-pass", []string{auth.ScopePlatformAdmin}},
		{"manager", "acct-manager@example.com", "acct-manager-pass", []string{"rag"}},
		{"member", "acct-member@example.com", "acct-member-pass", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := accountLogin(t, f, tc.email, tc.password, tc.scopes)
			sess, _ := login(t, f.app, tc.email, tc.password)
			rec := sessionDo(t, f.app, http.MethodGet, "/dashboard/account", "", sess, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("account page expected 200, got %d body=%s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			// §6.13 section order and the profile read from the service.
			for _, want := range []string{
				"Profile", "Change password", "Sessions", "Linked sign-ins",
				user.Email, "account.js",
			} {
				if !strings.Contains(body, want) {
					t.Errorf("render missing %q", want)
				}
			}
			// Roles render from the real read: the seeded role name embeds
			// the user id; a member without roles says so truthfully.
			if len(tc.scopes) > 0 {
				if !strings.Contains(body, user.ID) {
					t.Errorf("roles not rendered for %s", tc.email)
				}
			} else if !strings.Contains(body, "No roles assigned.") {
				t.Errorf("member without roles must say so, got %s", body)
			}
			// No admin surface anywhere in the page content. The admin
			// sidebar legitimately links /dashboard/access — scope the
			// check to <main>, never the chrome.
			mainStart := strings.Index(body, `<main class="main">`)
			if mainStart < 0 {
				t.Fatalf("account page main region missing, got %s", body)
			}
			mainEnd := strings.Index(body[mainStart:], "</main>")
			if mainEnd < 0 {
				t.Fatalf("account page main region unclosed, got %s", body)
			}
			content := body[mainStart : mainStart+mainEnd]
			for _, banned := range []string{"Create user", "Service accounts", "API keys", "/dashboard/access"} {
				if strings.Contains(content, banned) {
					t.Errorf("admin surface %q leaked onto the account page", banned)
				}
			}
		})
	}

	// A principal without a user record (service-account shape) renders the
	// designed 403 page — the page mirrors the API's own authority guard.
	req := httptest.NewRequest(http.MethodGet, "/dashboard/account", nil)
	ctx := withCaps(auth.WithPrincipal(req.Context(), auth.Principal{TenantID: f.tenant}), dashboardCaps{})
	rec := httptest.NewRecorder()
	f.app.handleDashboardAccountPage(rec, req.WithContext(ctx))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("principal without user record expected 403, got %d", rec.Code)
	}
}

func TestDashboardAccountUnauthenticated(t *testing.T) {
	f := newIdentityFixture(t)
	rec := f.do(t, http.MethodGet, "/dashboard/account", "", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated account page expected 303, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/dashboard/login") || !strings.Contains(loc, "return_to=%2Fdashboard%2Faccount") {
		t.Fatalf("login redirect must preserve return_to, got %q", loc)
	}
}

func TestDashboardAccountSessionsList(t *testing.T) {
	f := newIdentityFixture(t)
	user := accountLogin(t, f, "acct-sessions@example.com", "acct-sessions-pass", []string{"chatbot"})
	sess1, csrf := login(t, f.app, "acct-sessions@example.com", "acct-sessions-pass")
	_, _ = login(t, f.app, "acct-sessions@example.com", "acct-sessions-pass")

	sessions, err := f.repo.ListSessionsForUser(context.Background(), f.tenant, user.ID)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}

	rec := sessionDo(t, f.app, http.MethodGet, "/dashboard/partials/account/sessions", "", sess1, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sessions fragment expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, s := range sessions {
		if !strings.Contains(body, `data-account-revoke="`+s.ID+`"`) {
			t.Errorf("session %s missing its revoke control", s.ID)
		}
	}
	// The caller's own session is marked and signs out; the other revokes.
	if !strings.Contains(body, `data-account-current="1"`) || !strings.Contains(body, ">Sign out<") {
		t.Errorf("current session must be marked with a sign-out control, got %s", body)
	}
	if !strings.Contains(body, "Revoke this session?") {
		t.Errorf("revoke controls must carry the two-step confirm, got %s", body)
	}
	if !strings.Contains(body, `hx-get="/dashboard/partials/account/sessions"`) {
		t.Errorf("fragment must be refreshable, got %s", body)
	}

	// Revoke the NON-current session through the real API path; the current
	// one is identified by its marker on the same control.
	const attr = `data-account-revoke="`
	i := strings.Index(body, attr)
	if i < 0 {
		t.Fatal("no revoke control rendered")
	}
	rest := body[i+len(attr):]
	j := strings.Index(rest, `"`)
	if j < 0 || !strings.HasPrefix(rest[j:], `" data-account-current="1"`) {
		t.Fatalf("current marker must sit on the current session's control, got %s", body)
	}
	currentID := rest[:j]
	var other string
	for _, s := range sessions {
		if s.ID != currentID {
			other = s.ID
		}
	}
	if other == "" {
		t.Fatal("expected a non-current session to revoke")
	}
	rec = sessionDo(t, f.app, http.MethodDelete, "/api/v1/identities/me/sessions/"+other, "", sess1, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/partials/account/sessions", "", sess1, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sessions fragment after revoke expected 200, got %d", rec.Code)
	}
	body = rec.Body.String()
	if !strings.Contains(body, "Stopped") || !strings.Contains(body, "administrator revocation") {
		t.Errorf("revoked session must render its state and reason, got %s", body)
	}
	if strings.Contains(body, `data-account-revoke="`+other+`"`) {
		t.Errorf("revoked session must lose its revoke control")
	}
}

func TestDashboardAccountLinkForm(t *testing.T) {
	f := newIdentityFixture(t)
	accountLogin(t, f, "acct-link@example.com", "acct-link-pass", []string{"chatbot"})
	sess, _ := login(t, f.app, "acct-link@example.com", "acct-link-pass")

	// With a link_token the confirm form renders — and its password input
	// carries name="password" (the OA-12/§6.13 unnamed-input fix).
	rec := sessionDo(t, f.app, http.MethodGet, "/dashboard/account?link_token=tok-123", "", sess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("account page with link_token expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="account-link-form"`, `name="link_token"`, `value="tok-123"`,
		`id="account-link-password"`, `name="password"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("link form missing %q", want)
		}
	}

	// Without one there is no link form.
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/account", "", sess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("account page expected 200, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `id="account-link-form"`) {
		t.Errorf("link form must render only with a link_token, got %s", rec.Body.String())
	}
}
