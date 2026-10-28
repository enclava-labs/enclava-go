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
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
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

	sess, csrf := loginUser(t, f, "boss@example.com", []string{auth.ScopePlatformAdmin}, "admin-pass")

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
	readerSess, readerCSRF := loginUser(t, f, "reader@example.com", []string{"rag"}, "reader-pass")
	extractSess, extractCSRF := loginUser(t, f, "extract@example.com", []string{"extract"}, "extract-pass")
	adminSess, adminCSRF := loginUser(t, f, "admin2@example.com", []string{auth.ScopePlatformAdmin}, "admin-pass")
	pricerSess, pricerCSRF := loginUser(t, f, "pricer@example.com", []string{"pricing.manage"}, "pricer-pass")
	connSess, connCSRF := loginUser(t, f, "connector@example.com", []string{"connectors.manage"}, "conn-pass")

	// Member (chatbot only): every admin, provider, knowledge and extract
	// surface is refused — session alone never reaches the handler.
	memberDenied := []struct{ method, path string }{
		{http.MethodGet, "/dashboard/partials/keys"},
		{http.MethodGet, "/dashboard/partials/usage"},
		{http.MethodGet, "/dashboard/partials/providers"},
		{http.MethodGet, "/dashboard/partials/pricing"},
		{http.MethodGet, "/dashboard/partials/billing-audit"},
		{http.MethodGet, "/dashboard/partials/connectors"},
		{http.MethodGet, "/dashboard/partials/mcp"},
		{http.MethodGet, "/dashboard/partials/rag"},
		{http.MethodGet, "/dashboard/partials/extract"},
		{http.MethodGet, "/dashboard/partials/extract/templates"},
		{http.MethodGet, "/dashboard/partials/extract/jobs"},
		{http.MethodGet, "/dashboard/partials/extract/models"},
		{http.MethodGet, "/dashboard/partials/extract/settings"},
		{http.MethodGet, "/dashboard/rag/documents/doc-1/download"},
		{http.MethodGet, "/dashboard/keys"},
		{http.MethodGet, "/dashboard/analytics"},
		{http.MethodGet, "/dashboard/providers"},
		{http.MethodGet, "/dashboard/pricing"},
		{http.MethodGet, "/dashboard/billing-audit"},
		{http.MethodGet, "/dashboard/rag"},
		{http.MethodGet, "/dashboard/extract"},
		{http.MethodGet, "/dashboard/connectors"},
		{http.MethodGet, "/dashboard/mcp"},
		{http.MethodPost, "/dashboard/partials/extract/templates"},
		{http.MethodPut, "/dashboard/partials/extract/settings"},
		{http.MethodPut, "/dashboard/partials/extract/templates/t1"},
		{http.MethodPost, "/dashboard/partials/extract/reset-defaults"},
	}
	for _, tc := range memberDenied {
		rec := sessionForm(t, f.app, tc.method, tc.path, nil, memberSess, memberCSRF)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member %s %s: expected 403, got %d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}

	// Member keeps its own surfaces: anything but a scope refusal is fine
	// (unconfigured dependencies answer 501, configured ones render).
	memberOK := []struct{ method, path string }{
		{http.MethodGet, "/dashboard"},
		{http.MethodGet, "/dashboard/partials/stats"},
		{http.MethodGet, "/dashboard/budgets"},
		{http.MethodGet, "/dashboard/partials/budgets"},
		{http.MethodGet, "/dashboard/identity"},
		{http.MethodGet, "/dashboard/documents"},
		{http.MethodGet, "/dashboard/chatbots"},
		{http.MethodGet, "/dashboard/partials/chatbots"},
		{http.MethodGet, "/dashboard/prompt-templates"},
		{http.MethodGet, "/dashboard/partials/prompt-templates"},
	}
	for _, tc := range memberOK {
		rec := sessionForm(t, f.app, tc.method, tc.path, nil, memberSess, memberCSRF)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("member %s %s: unexpected 403", tc.method, tc.path)
		}
	}

	// Read-scope holders reach the read surfaces but mutations demand the
	// matching *.manage scope — enforced before the service lookup.
	post := url.Values{"action": {"create_collection"}}
	if rec := sessionForm(t, f.app, http.MethodPost, "/dashboard/partials/rag", post, readerSess, readerCSRF); rec.Code != http.StatusForbidden {
		t.Fatalf("rag post without rag.manage: expected 403, got %d", rec.Code)
	}
	if rec := sessionForm(t, f.app, http.MethodGet, "/dashboard/partials/rag", nil, readerSess, readerCSRF); rec.Code == http.StatusForbidden {
		t.Fatal("rag get with rag scope refused")
	}
	if rec := sessionForm(t, f.app, http.MethodGet, "/dashboard/rag", nil, readerSess, readerCSRF); rec.Code != http.StatusOK {
		t.Fatalf("rag page with rag scope: expected 200, got %d", rec.Code)
	}
	if rec := sessionForm(t, f.app, http.MethodGet, "/dashboard/rag/documents/doc-1/download", nil, readerSess, readerCSRF); rec.Code == http.StatusForbidden {
		t.Fatal("rag document download with rag scope refused")
	}

	if rec := sessionForm(t, f.app, http.MethodGet, "/dashboard/partials/extract", nil, extractSess, extractCSRF); rec.Code != http.StatusOK {
		t.Fatalf("extract partial with extract scope: expected 200, got %d", rec.Code)
	}
	if rec := sessionForm(t, f.app, http.MethodGet, "/dashboard/extract", nil, extractSess, extractCSRF); rec.Code != http.StatusOK {
		t.Fatalf("extract page with extract scope: expected 200, got %d", rec.Code)
	}
	if rec := sessionForm(t, f.app, http.MethodPost, "/dashboard/partials/extract/process", nil, extractSess, extractCSRF); rec.Code == http.StatusForbidden {
		t.Fatal("extract process with extract scope refused")
	}
	// chatbot-scope POSTs without chatbot.manage are refused inside the
	// handler, before the (unconfigured) service is consulted.
	if rec := sessionForm(t, f.app, http.MethodPost, "/dashboard/partials/chatbots", url.Values{"action": {"create_chatbot"}}, memberSess, memberCSRF); rec.Code != http.StatusForbidden {
		t.Fatalf("chatbot post without chatbot.manage: expected 403, got %d", rec.Code)
	}
	if rec := sessionForm(t, f.app, http.MethodPost, "/dashboard/partials/prompt-templates", url.Values{}, memberSess, memberCSRF); rec.Code != http.StatusForbidden {
		t.Fatalf("prompt-template post without chatbot.manage: expected 403, got %d", rec.Code)
	}

	// platform.admin reaches the admin surfaces (downstream nil services
	// answer 501 — never 403); provider/pricing and connector surfaces need
	// their own scopes, matching the API.
	adminOK := []struct{ method, path string }{
		{http.MethodGet, "/dashboard/partials/keys"},
		{http.MethodGet, "/dashboard/keys"},
		{http.MethodGet, "/dashboard/partials/usage"},
		{http.MethodGet, "/dashboard/analytics"},
		{http.MethodGet, "/dashboard/partials/billing-audit"},
		{http.MethodGet, "/dashboard/billing-audit"},
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
		rec := sessionForm(t, f.app, http.MethodGet, path, nil, connSess, connCSRF)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("connectors.manage GET %s: unexpected 403", path)
		}
	}
}

func TestDashboardStatsScopesKeyCountToCaller(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	// The fixture already seeds one active admin-owned key; add a second
	// admin key and one owned by the member.
	var fixtureAdminID string
	users, err := f.repo.ListUsers(ctx, f.tenant)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	for _, u := range users {
		if u.Email == "admin@example.com" {
			fixtureAdminID = u.ID
		}
	}
	if fixtureAdminID == "" {
		t.Fatal("fixture admin user missing")
	}
	member := testdb.SeedUser(t, f.db, f.tenant, "stat-member@example.com", nil)
	setPassword(t, f.repo, f.tenant, member.ID, "member-pass")
	testdb.SeedAPIKey(t, f.db, f.tenant, testdb.SeedKeySpec{Name: "admin-2", OwnerUserID: fixtureAdminID})
	testdb.SeedAPIKey(t, f.db, f.tenant, testdb.SeedKeySpec{Name: "member-1", OwnerUserID: member.ID})

	memberSess, _ := login(t, f.app, "stat-member@example.com", "member-pass")
	rec := sessionForm(t, f.app, http.MethodGet, "/dashboard/partials/stats", nil, memberSess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("member stats expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `<div class="stat-value">1</div>`) {
		t.Fatalf("member should see only their own active key, got %s", rec.Body.String())
	}

	admin := testdb.SeedUser(t, f.db, f.tenant, "stat-admin@example.com", []string{auth.ScopePlatformAdmin})
	setPassword(t, f.repo, f.tenant, admin.ID, "admin-pass")
	adminSess, _ := login(t, f.app, "stat-admin@example.com", "admin-pass")
	rec = sessionForm(t, f.app, http.MethodGet, "/dashboard/partials/stats", nil, adminSess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin stats expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `<div class="stat-value">3</div>`) {
		t.Fatalf("admin should see all three tenant keys, got %s", rec.Body.String())
	}
}

func TestDashboardBudgetsHideTenantSubjectFromMembers(t *testing.T) {
	f := newBudgetFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	memberSubject := billing.Subject{Type: auth.BillingSubjectUser, ID: "member-user"}
	tenantSubject := billing.Subject{Type: billing.BudgetSubjectTenant, ID: f.tenant}
	for i, subject := range []billing.Subject{memberSubject, tenantSubject} {
		if err := f.repo.CreateBudget(ctx, f.tenant, "b-"+subject.ID, billing.Budget{
			Name: "budget-" + subject.ID, Subject: subject, Kind: billing.BudgetKindAmount,
			Period: billing.PeriodMonthly, LimitAmountMicros: int64(100 + i), Currency: "USD",
			StartsAt: now.Add(-time.Hour),
		}); err != nil {
			t.Fatalf("create budget %s: %v", subject.ID, err)
		}
	}

	render := func(p auth.Principal) string {
		req := httptest.NewRequest(http.MethodGet, "/dashboard/partials/budgets", nil)
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
		rec := httptest.NewRecorder()
		f.app.handleDashboardBudgets(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("budgets render expected 200, got %d body=%s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	member := render(auth.Principal{
		TenantID: f.tenant, UserID: "member-user",
		BillingSubjectKind: auth.BillingSubjectUser, BillingSubjectID: "member-user",
		Scopes: map[string]struct{}{"chatbot": {}},
	})
	if !strings.Contains(member, "user member-user") {
		t.Fatalf("member should see their own budget row, got %s", member)
	}
	if strings.Contains(member, "tenant "+f.tenant) {
		t.Fatalf("member must not see the tenant-wide budget, got %s", member)
	}

	admin := render(auth.Principal{
		TenantID: f.tenant, UserID: "admin-user",
		BillingSubjectKind: auth.BillingSubjectUser, BillingSubjectID: "admin-user",
		Scopes: map[string]struct{}{auth.ScopePlatformAdmin: {}},
	})
	if !strings.Contains(admin, "tenant "+f.tenant) {
		t.Fatalf("admin should see the tenant-wide budget, got %s", admin)
	}
}
