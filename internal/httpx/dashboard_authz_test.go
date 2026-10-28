// dashboard_authz_test.go — authorization regression coverage on the
// post-cutover surface: new /dashboard/* pages and partials enforce their
// capability gates per role, /api/v1 endpoints keep their scope checks, and
// deleted old-surface URLs stay 404. Regression coverage for the report that
// any signed-in user could mint platform.admin keys, mutate every tenant
// key, and read admin pages.

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
	if session != nil {
		r.AddCookie(session)
	}
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

	// The legacy dashboard action-table died in the 09-10 sweep; the same
	// denial is asserted on the surviving admin API + Access surfaces.
	denied := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/v1/admin/api-keys", ""},
		{http.MethodPost, "/api/v1/admin/api-keys", `{"name":"escalated","scopes":["platform.admin"]}`},
		{http.MethodGet, "/dashboard/access/api-keys", ""},
		{http.MethodGet, "/dashboard/partials/access/api-keys", ""},
		{http.MethodGet, "/dashboard/partials/access/key-drawer", ""},
	}
	for _, tc := range denied {
		rec := sessionDo(t, f.app, tc.method, tc.path, tc.body, sess, csrf)
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
	rec := sessionDo(t, f.app, http.MethodPost, "/api/v1/admin/api-keys",
		`{"name":"admin-key","scopes":["platform.admin"],"allowed_models":["m1"],"allowed_endpoints":["e1"],"allowed_chatbots":["b1"],"allowed_agents":["b1"],"allowed_extract_templates":["t1"]}`, sess, csrf)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin create expected 201, got %d body=%s", rec.Code, rec.Body.String())
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
	rec = sessionDo(t, f.app, http.MethodPost, "/api/v1/admin/api-keys",
		`{"name":"too-wide","scopes":["platform.admin","made.up.scope"]}`, sess, csrf)
	if !strings.Contains(rec.Body.String(), "subset") {
		t.Fatalf("over-wide create expected subset refusal, got %d body=%s", rec.Code, rec.Body.String())
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

	// Member (chatbot only): every admin, models, sources, tools and
	// billing surface on the NEW route table is refused — session alone
	// never reaches the handler.
	memberDenied := []struct{ method, path string }{
		{http.MethodGet, "/dashboard/partials/usage/overview"},
		{http.MethodGet, "/dashboard/usage"},
		{http.MethodGet, "/dashboard/models"},
		{http.MethodGet, "/dashboard/partials/models/endpoints"},
		{http.MethodGet, "/dashboard/sources"},
		{http.MethodGet, "/dashboard/partials/sources/table"},
		{http.MethodGet, "/dashboard/tools"},
		{http.MethodGet, "/dashboard/partials/tools/queue"},
		{http.MethodGet, "/dashboard/access/api-keys"},
		{http.MethodGet, "/dashboard/partials/access/api-keys"},
	}
	for _, tc := range memberDenied {
		rec := sessionForm(t, f.app, tc.method, tc.path, nil, memberSess, memberCSRF)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member %s %s: expected 403, got %d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}

	// Member keeps its own surfaces: anything but a scope refusal is fine.
	// (The old /dashboard/identity page died in the 09-10 sweep — Account
	// owns the member self-service surface now.)
	memberOK := []struct{ method, path string }{
		{http.MethodGet, "/dashboard/account"},
	}
	for _, tc := range memberOK {
		rec := sessionForm(t, f.app, tc.method, tc.path, nil, memberSess, memberCSRF)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("member %s %s: unexpected 403", tc.method, tc.path)
		}
	}

	// Every deleted URL answers with the designed 404 — for members and
	// scope holders alike, with no redirect or alias (D-19). The rag
	// family died with the phase-10 Home cutover; the phase-9 page family
	// died in the 09-10 sweep.
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
		{http.MethodGet, "/dashboard/partials/keys"},
		{http.MethodPost, "/dashboard/partials/keys"},
		{http.MethodGet, "/dashboard/keys"},
		{http.MethodGet, "/dashboard/partials/providers"},
		{http.MethodGet, "/dashboard/providers"},
		{http.MethodGet, "/dashboard/partials/pricing"},
		{http.MethodGet, "/dashboard/pricing"},
		{http.MethodGet, "/dashboard/partials/connectors"},
		{http.MethodGet, "/dashboard/connectors"},
		{http.MethodGet, "/dashboard/partials/mcp"},
		{http.MethodGet, "/dashboard/mcp"},
		{http.MethodGet, "/dashboard/identity"},
		{http.MethodGet, "/dashboard/documents"},
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
		{http.MethodGet, "/dashboard/tools"},
		{http.MethodGet, "/dashboard/partials/tools/queue"},
		{http.MethodGet, "/dashboard/access"},
		{http.MethodGet, "/dashboard/partials/access/api-keys"},
		{http.MethodGet, "/dashboard/usage"},
		{http.MethodGet, "/dashboard/partials/usage/overview"},
	}
	for _, tc := range adminOK {
		rec := sessionForm(t, f.app, tc.method, tc.path, nil, adminSess, adminCSRF)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("admin %s %s: unexpected 403", tc.method, tc.path)
		}
	}
	// Scope checks are exact: platform.admin alone does not open
	// pricing.manage or connectors.manage surfaces.
	for _, path := range []string{"/dashboard/models", "/dashboard/partials/models/endpoints", "/dashboard/sources", "/dashboard/partials/sources/table"} {
		rec := sessionForm(t, f.app, http.MethodGet, path, nil, adminSess, adminCSRF)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("admin without manage scope %s: expected 403, got %d", path, rec.Code)
		}
	}
	pricerOK := []struct{ method, path string }{
		{http.MethodGet, "/dashboard/models"},
		{http.MethodGet, "/dashboard/partials/models/endpoints"},
		{http.MethodGet, "/dashboard/models/pricing"},
		{http.MethodGet, "/dashboard/partials/models/pricing"},
	}
	for _, tc := range pricerOK {
		rec := sessionForm(t, f.app, tc.method, tc.path, nil, pricerSess, pricerCSRF)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("pricing.manage %s %s: unexpected 403", tc.method, tc.path)
		}
	}
	for _, path := range []string{"/dashboard/sources", "/dashboard/partials/sources/table"} {
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
