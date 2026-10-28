// dashboard_authz_test.go — dashboard session routes must enforce the same
// scopes as the equivalent /api/v1 endpoints: key management, tenant-wide
// reads and *.manage mutations are refused for members that only hold a
// session. Regression coverage for the report that any signed-in user could
// mint platform.admin keys, mutate every tenant key, and read admin pages.

package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/extract"
	"enclava-go/internal/testdb"
)

// sessionForm replays a session request through the real mux, form-encoding
// the body and attaching the CSRF cookie+header the way dashboard JS does.
func sessionForm(t *testing.T, app *App, method, path string, form url.Values, session, csrf *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.AddCookie(session)
	if csrf != nil && method != http.MethodGet && method != http.MethodHead {
		r.AddCookie(csrf)
		r.Header.Set("X-CSRF-Token", csrf.Value)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, r)
	return rec
}

// loginUser seeds a scoped user with a known password and returns its
// session cookies.
func loginUser(t *testing.T, f *identityFixture, email string, scopes []string, password string) (*http.Cookie, *http.Cookie) {
	t.Helper()
	user := testdb.SeedUser(t, f.db, f.tenant, email, scopes)
	setPassword(t, f.repo, f.tenant, user.ID, password)
	sess, csrf := login(t, f.app, email, password)
	return sess, csrf
}

// loginUserWithGrants seeds a scoped user whose role carries the supplied
// stored resource grants and returns its session cookies — the helper for
// member/manager sessions needing real list-grant authority.
func loginUserWithGrants(t *testing.T, f *identityFixture, email string, scopes []string, grants auth.ResourceGrants, password string) (*http.Cookie, *http.Cookie) {
	t.Helper()
	user := testdb.SeedUserWithGrants(t, f.db, f.tenant, email, scopes, grants)
	setPassword(t, f.repo, f.tenant, user.ID, password)
	sess, csrf := login(t, f.app, email, password)
	return sess, csrf
}

func TestDashboardKeyManagementRequiresPlatformAdmin(t *testing.T) {
	f := newIdentityFixture(t)
	sess, csrf := loginUser(t, f, "member@example.com", []string{"chatbot", "models.list"}, "member-pass")

	denied := []struct {
		method string
		path   string
		form   url.Values
	}{
		{http.MethodGet, "/dashboard/partials/keys", nil},
		{http.MethodPost, "/dashboard/partials/keys", url.Values{"action": {"create"}, "name": {"escalated"}, "scopes": {"platform.admin"}}},
		{http.MethodPost, "/dashboard/partials/keys", url.Values{"action": {"toggle"}, "key_id": {"any"}}},
		{http.MethodPost, "/dashboard/partials/keys", url.Values{"action": {"delete"}, "key_id": {"any"}}},
		{http.MethodPost, "/dashboard/partials/keys", url.Values{"action": {"regenerate"}, "key_id": {"any"}}},
		{http.MethodGet, "/dashboard/keys", nil},
	}
	for _, tc := range denied {
		rec := sessionForm(t, f.app, tc.method, tc.path, tc.form, sess, csrf)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s: expected 403 for member, got %d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}

	// Nothing was minted: only the fixture's seeded key exists.
	keys, err := f.repo.ListAPIKeys(context.Background(), f.tenant)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected only the seeded key after denied writes, got %d", len(keys))
	}
}

func TestDashboardKeyCreateScopeSubsetAndDefaults(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	// The hidden form field no longer ships platform.admin or *.manage
	// scopes as the silent default.
	for _, s := range dashboardDefaultScopes {
		if s == auth.ScopePlatformAdmin || strings.HasSuffix(s, ".manage") {
			t.Fatalf("dashboard default scopes still grant %q", s)
		}
	}

	// The permitted "boss" issuer carries explicit mode:all grants — an
	// unrestricted issuer keeps the legacy omitted→all mint behavior, while
	// zero-grant admin denial is covered by the new grants-bound deny cases.
	bossGrants := auth.ResourceGrants{
		Models:           auth.Allowlist{All: true},
		Endpoints:        auth.Allowlist{All: true},
		Assistants:       auth.Allowlist{All: true},
		ExtractTemplates: auth.Allowlist{All: true},
	}
	sess, csrf := loginUserWithGrants(t, f, "boss@example.com", []string{auth.ScopePlatformAdmin}, bossGrants, "admin-pass")

	// A scope the caller holds mints fine.
	rec := sessionForm(t, f.app, http.MethodPost, "/dashboard/partials/keys",
		url.Values{"action": {"create"}, "name": {"admin-key"}, "scopes": {"platform.admin"}}, sess, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin create expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	keys, err := f.repo.ListAPIKeys(ctx, f.tenant)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	var created *auth.APIKeyRecord
	for i, k := range keys {
		if k.Name == "admin-key" {
			created = &keys[i]
		}
	}
	if created == nil {
		t.Fatalf("expected minted key, have %v", keys)
	}
	if items, _ := auth.DecodeAllowlist(created.Scopes); len(items) != 1 {
		t.Fatalf("minted scopes = %s, expected exactly platform.admin", created.Scopes)
	} else if _, ok := items[auth.ScopePlatformAdmin]; !ok {
		t.Fatalf("minted scopes = %s, expected platform.admin", created.Scopes)
	}

	// A scope the caller does not hold is refused — and nothing persists.
	rec = sessionForm(t, f.app, http.MethodPost, "/dashboard/partials/keys",
		url.Values{"action": {"create"}, "name": {"too-wide"}, "scopes": {"platform.admin,made.up.scope"}}, sess, csrf)
	if !strings.Contains(rec.Body.String(), "subset") {
		t.Fatalf("over-wide create expected subset refusal, got %d body=%s", rec.Code, rec.Body.String())
	}

	// The form's default scope set is also refused for a caller that lacks
	// it — same rule as the admin API route.
	rec = sessionForm(t, f.app, http.MethodPost, "/dashboard/partials/keys",
		url.Values{"action": {"create"}, "name": {"defaults"}}, sess, csrf)
	if !strings.Contains(rec.Body.String(), "subset") {
		t.Fatalf("default-scope create expected subset refusal, got %d body=%s", rec.Code, rec.Body.String())
	}

	keys, err = f.repo.ListAPIKeys(ctx, f.tenant)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("refused creates persisted keys, have %d rows", len(keys))
	}
}

func TestDashboardEndpointsMirrorAPIScopes(t *testing.T) {
	f := newIdentityFixture(t)

	memberSess, memberCSRF := loginUser(t, f, "member@example.com", []string{"chatbot"}, "member-pass")
	readerSess, _ := loginUser(t, f, "reader@example.com", []string{"rag"}, "reader-pass")
	adminSess, adminCSRF := loginUser(t, f, "admin2@example.com", []string{auth.ScopePlatformAdmin}, "admin-pass")
	pricerSess, pricerCSRF := loginUser(t, f, "pricer@example.com", []string{"pricing.manage"}, "pricer-pass")
	connSess, _ := loginUser(t, f, "connector@example.com", []string{"connectors.manage"}, "conn-pass")

	// Member (chatbot only): every admin, provider, knowledge and extract
	// surface is refused — session alone never reaches the handler.
	// (Budgets/usage/analytics/billing-audit moved to the admin-only
	// /dashboard/usage surface in 09-08's cutover; the rag family died with
	// the phase-10 teardown and moved to the 404 block.)
	memberDenied := []struct{ method, path string }{
		{http.MethodGet, "/dashboard/partials/keys"},
		{http.MethodGet, "/dashboard/partials/usage/overview"},
		{http.MethodGet, "/dashboard/partials/providers"},
		{http.MethodGet, "/dashboard/partials/pricing"},
		{http.MethodGet, "/dashboard/partials/connectors"},
		{http.MethodGet, "/dashboard/partials/mcp"},
		{http.MethodGet, "/dashboard/keys"},
		{http.MethodGet, "/dashboard/providers"},
		{http.MethodGet, "/dashboard/pricing"},
		{http.MethodGet, "/dashboard/connectors"},
		{http.MethodGet, "/dashboard/mcp"},
	}
	for _, tc := range memberDenied {
		rec := sessionForm(t, f.app, tc.method, tc.path, nil, memberSess, memberCSRF)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member %s %s: expected 403, got %d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}

	// Member keeps its own surfaces: anything but a scope refusal is fine.
	// (Budgets/usage moved to the admin-only /dashboard/usage surface in
	// 09-08's cutover; the rag/chatbots/prompt-templates/stats family died
	// with the phase-10 Home cutover — see the 404 block below.)
	memberOK := []struct{ method, path string }{
		{http.MethodGet, "/dashboard/identity"},
	}
	for _, tc := range memberOK {
		rec := sessionForm(t, f.app, tc.method, tc.path, nil, memberSess, memberCSRF)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("member %s %s: unexpected 403", tc.method, tc.path)
		}
	}

	// The old rag family died with the phase-10 Home cutover: every deleted
	// URL answers with the designed 404 — for members and scope holders
	// alike, with no redirect or alias (D-19).
	deletedRoutes := []struct{ method, path string }{
		{http.MethodGet, "/dashboard/partials/stats"},
		{http.MethodGet, "/dashboard/chatbots"},
		{http.MethodGet, "/dashboard/partials/chatbots"},
		{http.MethodPost, "/dashboard/partials/chatbots"},
		{http.MethodGet, "/dashboard/prompt-templates"},
		{http.MethodGet, "/dashboard/partials/prompt-templates"},
		{http.MethodGet, "/dashboard/rag"},
		{http.MethodGet, "/dashboard/partials/rag"},
		{http.MethodGet, "/dashboard/rag/documents/doc-1/download"},
	}
	for _, tc := range deletedRoutes {
		pairs := []struct{ session, csrf *http.Cookie }{{memberSess, memberCSRF}}
		if tc.method == http.MethodGet { // a reader's bare GET still 404s; POSTs need their matching CSRF pair
			pairs = append(pairs, struct{ session, csrf *http.Cookie }{readerSess, nil})
		}
		for _, pair := range pairs {
			rec := sessionForm(t, f.app, tc.method, tc.path, nil, pair.session, pair.csrf)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("deleted %s %s: expected the designed 404, got %d body=%s",
					tc.method, tc.path, rec.Code, rec.Body.String())
			}
		}
	}

	// platform.admin reaches the admin surfaces (downstream nil services
	// answer 501 — never 403).
	adminOK := []struct{ method, path string }{
		{http.MethodGet, "/dashboard/partials/keys"},
		{http.MethodGet, "/dashboard/keys"},
		{http.MethodGet, "/dashboard/partials/mcp"},
		{http.MethodGet, "/dashboard/mcp"},
	}
	for _, tc := range adminOK {
		rec := sessionForm(t, f.app, tc.method, tc.path, nil, adminSess, adminCSRF)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("admin %s %s: unexpected 403", tc.method, tc.path)
		}
	}
	// Scope checks are exact: platform.admin alone does not open
	// pricing.manage or connectors.manage surfaces.
	for _, path := range []string{"/dashboard/partials/providers", "/dashboard/partials/pricing", "/dashboard/partials/connectors"} {
		rec := sessionForm(t, f.app, http.MethodGet, path, nil, adminSess, adminCSRF)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("admin without %s scope %s: expected 403, got %d", "manage", path, rec.Code)
		}
	}
	pricerOK := []struct{ method, path string }{
		{http.MethodGet, "/dashboard/partials/providers"},
		{http.MethodGet, "/dashboard/providers"},
		{http.MethodGet, "/dashboard/partials/pricing"},
		{http.MethodGet, "/dashboard/pricing"},
	}
	for _, tc := range pricerOK {
		rec := sessionForm(t, f.app, tc.method, tc.path, nil, pricerSess, pricerCSRF)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("pricing.manage %s %s: unexpected 403", tc.method, tc.path)
		}
	}
	for _, path := range []string{"/dashboard/partials/connectors", "/dashboard/connectors"} {
		rec := sessionForm(t, f.app, http.MethodGet, path, nil, connSess, nil)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("connectors.manage GET %s: unexpected 403", path)
		}
	}
}

// TestDashboardHomeScopesOwnStatsToCaller replaces the deleted stats-trio
// coverage (TestDashboardStatsScopesKeyCountToCaller — the surface died with
// the phase-10 Home cutover): the new Home is actor-scoped the same way — a
// member's extraction total and running feed count only their OWN jobs, and
// another member's jobs never appear, whatever their counts.
func TestDashboardHomeScopesOwnStatsToCaller(t *testing.T) {
	f := newHomeFixture(t)

	other, _ := homeUser(t, f, "stat-other@example.com", []string{"extract"}, auth.ResourceGrants{}, "other-pass")
	f.enqueue(t, other, extract.OperationProcess, "stat-other-1")
	f.enqueue(t, other, extract.OperationProcess, "stat-other-2")

	member, memberSess := homeUser(t, f, "stat-member@example.com", []string{"extract"}, auth.ResourceGrants{}, "member-pass")
	f.enqueue(t, member, extract.OperationProcess, "stat-member-1")

	body := homeGet(t, f, "/dashboard", memberSess)
	if !strings.Contains(body, "1 total") {
		t.Fatalf("member extraction total must scope to own jobs, not the tenant's\n%s", body)
	}
	frag := homeGet(t, f, "/dashboard/partials/home/running", memberSess)
	if got := strings.Count(frag, `class="activity-row"`); got != 1 {
		t.Fatalf("member running feed must show exactly their own job, got %d rows\n%s", got, frag)
	}
	if !strings.Contains(frag, "every 3s") {
		t.Fatal("own queued job must keep the fragment polling")
	}
}

// The member-readable /dashboard/partials/budgets surface and its scoped
// visibility rules died with plan 09-08's atomic billing cutover — budgets
// are admin-only now (TestDashboardUsageMemberDenied covers the 403s).
