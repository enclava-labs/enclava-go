// dashboard_access_test.go — render + bounded-authority tests for the Access
// page (09-07, OA-28): five-tab gating per role, the D-25 roles editor
// (deny-all default, mode:all only for the seeded Admin, plain-word
// capabilities, id-only endpoint summaries), the role delete/assign error
// surfaces and the key drawer's scope bound. Sessions run the real login +
// CSRF path over the real migrated schema; there are no mocked repositories.
//
// Obsolete-surface handoff for 09-10 (this page replaced them; do not delete
// before their consumers die with them): in dashboard.go —
// handleDashboardPageKeys, handleDashboardAPIKeys (+ handleDashboardAPIKeysAction
// if present), the /dashboard/keys and /dashboard/partials/keys registrations;
// templates page_keys.tmpl and partial_keys.tmpl. The admin half of
// handleDashboardPageIdentity is 09-07-replaced too, but the whole identity
// page + /dashboard/identity registration is deleted only once 09-09 (Account)
// lands — shared deletion, one owner: 09-10.
package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/connectors"
	"enclava-go/internal/extract"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

// accessAdminScopes is the seeded Admin's explicit scope set — platform.admin
// implies no feature scopes, so every capability is granted by name (§3.2).
var accessAdminScopes = []string{
	"chatbot", "chatbot.manage", "rag", "rag.manage", "connectors.manage",
	"extract", "extract.manage", "pricing.manage", "models.list",
	"chat.completions", "embeddings.create", "responses", auth.ScopePlatformAdmin,
}

// accessEmptyGrants is the canonical explicit EMPTY (deny) grant document set
// the seeded Member and Knowledge-manager roles carry (D-25).
func accessEmptyGrants() auth.ResourceGrants {
	return auth.ResourceGrants{
		Models:           auth.Allowlist{Items: []string{}},
		Endpoints:        auth.Allowlist{Items: []string{}},
		Assistants:       auth.Allowlist{Items: []string{}},
		ExtractTemplates: auth.Allowlist{Items: []string{}},
	}
}

type accessFixture struct {
	app    *App
	repo   *postgres.IdentityRepo
	db     *postgres.DB
	tenant string
}

func newAccessFixture(t *testing.T) *accessFixture {
	t.Helper()
	db := testdb.New(t)
	repo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "bootstrap")
	ctx := context.Background()

	// Seeded Admin: explicit all-mode grants — the only role that may hold
	// mode:all (D-25).
	admin := testdb.SeedUserWithGrants(t, db, tenant, "admin@example.com", accessAdminScopes, auth.ResourceGrants{
		Models:           auth.Allowlist{All: true},
		Endpoints:        auth.Allowlist{All: true},
		Assistants:       auth.Allowlist{All: true},
		ExtractTemplates: auth.Allowlist{All: true},
	})
	setPassword(t, repo, tenant, admin.ID, "admin-pass-1")

	member := testdb.SeedUser(t, db, tenant, "member@example.com", []string{"chatbot", "rag"})
	setPassword(t, repo, tenant, member.ID, "member-pass-1")
	memberRole := auth.Role{ID: "role_member_fix", Name: "Member", Scopes: []string{"chatbot", "rag"}, Grants: accessEmptyGrants()}
	if err := repo.CreateRole(ctx, tenant, memberRole); err != nil {
		t.Fatalf("seed member role: %v", err)
	}
	if err := repo.AssignRole(ctx, tenant, member.ID, memberRole.ID); err != nil {
		t.Fatalf("assign member role: %v", err)
	}

	// Knowledge manager: manage scopes but NO platform.admin → access.manage
	// denied; explicit empty grants like the Member role.
	km := testdb.SeedUserWithGrants(t, db, tenant, "km@example.com",
		[]string{"chatbot", "rag", "rag.manage", "connectors.manage", "chatbot.manage", "extract.manage"}, accessEmptyGrants())
	setPassword(t, repo, tenant, km.ID, "km-pass-1")

	authSvc := auth.NewService(repo, "")
	sessionAuth := NewSessionAuth(authSvc, auth.NewOIDCService(repo, nil), time.Hour, DashboardAuthTemplates(), "bootstrap")
	jobsRepo := postgres.NewJobsRepo(db)
	jobsSvc := jobs.NewService(jobsRepo, 0)
	sourceRepo := postgres.NewSourceRepo(db)
	ragSvc := rag.NewService(postgres.NewRAGRepo(db), nil, nil, "", nil, jobsSvc)
	chatSvc := chatbot.NewService(postgres.NewChatbotRepo(db), postgres.NewPromptsRepo(db), ragSvc, nil)
	promptSvc := prompts.NewService(postgres.NewPromptsRepo(db), chatSvc)
	connectorSvc := connectors.NewService(sourceRepo, jobsSvc, jobsRepo)
	endpoints := postgres.NewEndpointsRepo(db)
	extractSvc := extract.NewService(postgres.NewExtractRepo(db), postgres.NewExtractRepo(db), jobsRepo, jobsSvc, nil, nil, endpoints, 5)
	app := NewApp(authSvc, nil, "", nil, repo, extractSvc, ragSvc, promptSvc, chatSvc, connectorSvc, nil, nil, endpoints,
		"sha256", "", "enc_", nil, sessionAuth, 1, false, nil, nil, GateConfig{}, sourceRepo, postgres.NewArtifactRepo(db))
	return &accessFixture{app: app, repo: repo, db: db, tenant: tenant}
}

// loginAs performs the real browser login for a fixture user.
func (f *accessFixture) loginAs(t *testing.T, email, password string) (session, csrf *http.Cookie) {
	t.Helper()
	return login(t, f.app, email, password)
}

func (f *accessFixture) do(t *testing.T, session, csrf *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return sessionDo(t, f.app, method, path, body, session, csrf)
}

// fixtureUser resolves a seeded user by email.
func (f *accessFixture) fixtureUser(t *testing.T, email string) auth.User {
	t.Helper()
	users, err := f.repo.ListUsers(context.Background(), f.tenant)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	for _, u := range users {
		if u.Email == email {
			return u
		}
	}
	t.Fatalf("fixture user %q not found", email)
	return auth.User{}
}

// seedPickerRows gives the grant pickers real tenant rows: one endpoint, one
// assistant and one extract template — each carrying fields that must NEVER
// reach the picker markup (origin, credential ref).
func (f *accessFixture) seedPickerRows(t *testing.T) (endpointID, botID, templateID string) {
	t.Helper()
	ctx := context.Background()
	endpointID, botID, templateID = "ep_alpha", "bot_alpha", "tpl_invoice"
	if err := postgres.NewEndpointsRepo(f.db).Create(ctx, inference.Endpoint{ //nolint:gosec // Synthetic endpoint and credential reference for a picker test.
		ID: endpointID, TenantID: f.tenant, Protocol: "openai", SDKProvider: "openai",
		Capabilities: []string{"chat:" + endpointID + ":m1"}, Enabled: true,
		BaseOrigin: "https://secret-origin.example", CredentialRef: "cred_secret_ref",
	}); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
	if err := postgres.NewChatbotRepo(f.db).Create(ctx, chatbot.Instance{
		ID: botID, TenantID: f.tenant, ExternalID: botID, Name: "Alpha assistant", Model: "m1",
	}); err != nil {
		t.Fatalf("seed chatbot: %v", err)
	}
	if err := postgres.NewExtractRepo(f.db).UpsertTemplate(ctx, f.tenant, extract.Template{
		ID: templateID, Name: "Invoice extraction", SystemPrompt: "s", UserPrompt: "u", Enabled: true,
	}); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	return endpointID, botID, templateID
}

func mustContain(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Fatalf("expected %q in body:\n%s", want, body)
	}
}

func mustNotContain(t *testing.T, body, want string) {
	t.Helper()
	if strings.Contains(body, want) {
		t.Fatalf("did not expect %q in body:\n%s", want, body)
	}
}

// TestDashboardAccessMemberDenied: member and manager get the 403 page on
// every tab route and an empty 403 on every partial/drawer route.
func TestDashboardAccessMemberDenied(t *testing.T) {
	f := newAccessFixture(t)
	routes := []string{
		"/dashboard/access",
		"/dashboard/access/people",
		"/dashboard/access/roles",
		"/dashboard/access/service-accounts",
		"/dashboard/access/api-keys",
		"/dashboard/access/sign-in",
		"/dashboard/partials/access/people",
		"/dashboard/partials/access/roles",
		"/dashboard/partials/access/service-accounts",
		"/dashboard/partials/access/api-keys",
		"/dashboard/partials/access/sign-in",
		"/dashboard/partials/access/user-drawer",
		"/dashboard/partials/access/role-editor",
		"/dashboard/partials/access/key-drawer",
	}
	for _, user := range []struct{ email, password string }{
		{"member@example.com", "member-pass-1"},
		{"km@example.com", "km-pass-1"},
	} {
		session, csrf := f.loginAs(t, user.email, user.password)
		for _, path := range routes {
			rec := f.do(t, session, csrf, http.MethodGet, path, "")
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s GET %s: expected 403, got %d", user.email, path, rec.Code)
			}
		}
	}
}

// TestDashboardAccessTabs: the five URLs resolve for an admin, exactly one
// aria-current per page, /dashboard/access renders People with the seeded
// member's real row, and the Sign-in tab carries zero form controls.
func TestDashboardAccessTabs(t *testing.T) {
	f := newAccessFixture(t)
	session, csrf := f.loginAs(t, "admin@example.com", "admin-pass-1")
	for _, tab := range []string{"", "/people", "/roles", "/service-accounts", "/api-keys", "/sign-in"} {
		rec := f.do(t, session, csrf, http.MethodGet, "/dashboard/access"+tab, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /dashboard/access%s: expected 200, got %d body=%s", tab, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		// aria-current appears once in the sidebar nav and once inside the
		// page's tabs nav — assert exactly one within the tabs region.
		start := strings.Index(body, `<nav class="tabs"`)
		if start < 0 {
			t.Fatalf("GET /dashboard/access%s: tabs nav missing", tab)
		}
		end := strings.Index(body[start:], "</nav>")
		if end < 0 {
			t.Fatalf("GET /dashboard/access%s: tabs nav unclosed", tab)
		}
		if got := strings.Count(body[start:start+end], `aria-current="page"`); got != 1 {
			t.Fatalf("GET /dashboard/access%s: expected exactly one aria-current in the tabs, got %d", tab, got)
		}
	}
	rec := f.do(t, session, csrf, http.MethodGet, "/dashboard/access", "")
	body := rec.Body.String()
	mustContain(t, body, `id="access-people"`)
	mustContain(t, body, "member@example.com")
	mustContain(t, body, "Manage")

	rec = f.do(t, session, csrf, http.MethodGet, "/dashboard/access/sign-in", "")
	// Read-only tab: no forms or inputs in the tab's own container. The
	// shell's logout form is legitimate chrome on every page, so scope to the
	// fragment region rather than the whole body.
	signIn := rec.Body.String()
	if start := strings.Index(signIn, `id="access-sign-in"`); start >= 0 {
		rest := signIn[start:]
		if end := strings.Index(rest, `</main>`); end >= 0 {
			rest = rest[:end]
		}
		signIn = rest
	}
	mustNotContain(t, signIn, "<form")
	mustNotContain(t, signIn, "<input")
}

// TestDashboardAccessRoleEditor: the roles editor renders scopes as plain-word
// capabilities, bounded id-only grant items, deny-all for the empty-grant
// Member role and the new-role default, and mode:all prechecked only for the
// seeded Admin role — never provider origins or credential refs.
func TestDashboardAccessRoleEditor(t *testing.T) {
	f := newAccessFixture(t)
	endpointID, botID, templateID := f.seedPickerRows(t)
	session, csrf := f.loginAs(t, "admin@example.com", "admin-pass-1")

	// Roles list: member counts + plain-word capabilities, never raw scopes.
	rec := f.do(t, session, csrf, http.MethodGet, "/dashboard/access/roles", "")
	mustContain(t, rec.Body.String(), "Chat · Search")
	mustNotContain(t, rec.Body.String(), `data-label="Capabilities">chatbot`)

	// Member role editor: explicit deny state everywhere.
	rec = f.do(t, session, csrf, http.MethodGet, "/dashboard/partials/access/role-editor?id=role_member_fix", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("role editor: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, kind := range []string{"models", "endpoints", "assistants", "extract_templates"} {
		mustContain(t, body, `data-grant="`+kind+`"`)
	}
	mustNotContain(t, body, "data-grant-all checked")
	mustNotContain(t, body, `data-grant-item value="`+templateID+`" checked`)
	mustNotContain(t, body, `data-grant-item value="`+endpointID+`" checked`)
	mustContain(t, body, "intersection of this role's grants")

	// Grantable summaries carry ids/display names only — never the endpoint's
	// origin or credential ref (D-25).
	mustContain(t, body, `data-grant-item value="`+endpointID+`"`)
	mustContain(t, body, `data-grant-item value="`+botID+`"`)
	mustContain(t, body, `data-grant-item value="`+templateID+`"`)
	mustNotContain(t, body, "secret-origin.example")
	mustNotContain(t, body, "cred_secret_ref")

	// Seeded Admin role: mode:all prechecked in all four sections.
	roles, err := f.repo.ListRoles(context.Background(), f.tenant)
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	adminRoleID := ""
	for _, r := range roles {
		if strings.HasPrefix(r.Name, "seeded-") && r.Grants.Models.All {
			adminRoleID = r.ID
		}
	}
	if adminRoleID == "" {
		t.Fatalf("seeded admin role not found among %d roles", len(roles))
	}
	rec = f.do(t, session, csrf, http.MethodGet, "/dashboard/partials/access/role-editor?id="+adminRoleID, "")
	if got := strings.Count(rec.Body.String(), "data-grant-all checked"); got != 4 {
		t.Fatalf("expected 4 prechecked All toggles for the Admin role, got %d", got)
	}

	// New non-admin role: every section renders deny-all — All off, nothing
	// checked (D-25 default).
	rec = f.do(t, session, csrf, http.MethodGet, "/dashboard/partials/access/role-editor", "")
	body = rec.Body.String()
	mustNotContain(t, body, "data-grant-all checked")
	mustNotContain(t, body, `data-grant-item value="`+endpointID+`" checked`)
	mustNotContain(t, body, `data-grant-item value="`+botID+`" checked`)
	mustNotContain(t, body, `data-grant-item value="`+templateID+`" checked`)
}

// TestDashboardAccessRoleDelete409: a membered role refuses deletion with the
// surfaced role_has_members error, the roles tab wires the destructive control
// to the real delete route, and an unused role deletes cleanly.
func TestDashboardAccessRoleDelete409(t *testing.T) {
	f := newAccessFixture(t)
	session, csrf := f.loginAs(t, "admin@example.com", "admin-pass-1")
	ctx := context.Background()

	rec := f.do(t, session, csrf, http.MethodGet, "/dashboard/access/roles", "")
	mustContain(t, rec.Body.String(), `data-access-confirm="role-delete"`)
	mustContain(t, rec.Body.String(), `data-role-id="role_member_fix"`)

	rec = f.do(t, session, csrf, http.MethodDelete, "/api/v1/identities/roles/role_member_fix", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete membered role: expected 409, got %d body=%s", rec.Code, rec.Body.String())
	}
	mustContain(t, rec.Body.String(), "role_has_members")

	member := f.fixtureUser(t, "member@example.com")
	if err := f.repo.UnassignRoleAtomically(ctx, f.tenant, member.ID, "role_member_fix"); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	rec = f.do(t, session, csrf, http.MethodDelete, "/api/v1/identities/roles/role_member_fix", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete unused role: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestDashboardAccessRoleAssign: the drawer's role checklist persists through
// PUT …/users/{id}/roles/{roleID}, and a role carrying scopes beyond the
// caller's own is refused with the assignRoleChecked 403.
func TestDashboardAccessRoleAssign(t *testing.T) {
	f := newAccessFixture(t)
	session, csrf := f.loginAs(t, "admin@example.com", "admin-pass-1")
	ctx := context.Background()

	member := f.fixtureUser(t, "member@example.com")
	extra := auth.Role{ID: "role_extra_fix", Name: "Reviewer", Scopes: []string{"rag.manage"}, Grants: accessEmptyGrants()}
	if err := f.repo.CreateRole(ctx, f.tenant, extra); err != nil {
		t.Fatalf("seed extra role: %v", err)
	}
	rec := f.do(t, session, csrf, http.MethodPut, "/api/v1/identities/users/"+member.ID+"/roles/"+extra.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("assign role: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	// Persisted outcome: the People tab shows the member under the new role.
	rec = f.do(t, session, csrf, http.MethodGet, "/dashboard/access/people", "")
	mustContain(t, rec.Body.String(), "Reviewer")

	// Out-of-authority fixture: an admin holding platform.admin + rag only
	// cannot assign a role carrying pricing.manage — assignRoleChecked 403s.
	limited := testdb.SeedUser(t, f.db, f.tenant, "limited-admin@example.com", []string{auth.ScopePlatformAdmin, "rag"})
	setPassword(t, f.repo, f.tenant, limited.ID, "limited-pass-1")
	wider := auth.Role{ID: "role_wider_fix", Name: "Wider", Scopes: []string{"pricing.manage"}}
	if err := f.repo.CreateRole(ctx, f.tenant, wider); err != nil {
		t.Fatalf("seed wider role: %v", err)
	}
	lsess, lcsrf := f.loginAs(t, "limited-admin@example.com", "limited-pass-1")
	rec = f.do(t, lsess, lcsrf, http.MethodPut, "/api/v1/identities/users/"+member.ID+"/roles/"+wider.ID, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("out-of-authority assign: expected 403, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestDashboardAccessKeyScopeBound: the key drawer checklist stays inside the
// creator's own scopes, rotate/revoke wire to the real subroutes, and
// out-of-authority issuance 403s API-side (principalCoversScopes).
func TestDashboardAccessKeyScopeBound(t *testing.T) {
	f := newAccessFixture(t)

	// A platform.admin+rag admin sees only their own scopes in the checklist.
	limited := testdb.SeedUser(t, f.db, f.tenant, "limited-admin@example.com", []string{auth.ScopePlatformAdmin, "rag"})
	setPassword(t, f.repo, f.tenant, limited.ID, "limited-pass-1")
	lsess, lcsrf := f.loginAs(t, "limited-admin@example.com", "limited-pass-1")
	rec := f.do(t, lsess, lcsrf, http.MethodGet, "/dashboard/partials/access/key-drawer", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("key drawer: expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body, `value="rag"`)
	mustContain(t, body, `value="platform.admin"`)
	mustNotContain(t, body, `value="chatbot"`)

	// Out-of-authority issuance: scopes beyond the caller's own 403 — the
	// drawer always submits scopes, so this is exactly the residual abuse.
	rec = f.do(t, lsess, lcsrf, http.MethodPost, "/api/v1/admin/api-keys", `{"name":"sneaky","scopes":["chatbot"]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("out-of-authority key create: expected 403, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Full admin: rotate + revoke controls target the real subroutes; the
	// rotated secret arrives once; revoke removes the row from the view.
	session, csrf := f.loginAs(t, "admin@example.com", "admin-pass-1")
	plaintext, key := testdb.SeedAPIKey(t, f.db, f.tenant, testdb.SeedKeySpec{
		Name: "rotate-me", OwnerUserID: limited.ID, Scopes: []string{"rag"},
	})
	if plaintext == "" || key.ID == "" {
		t.Fatalf("seed key failed")
	}
	rec = f.do(t, session, csrf, http.MethodGet, "/dashboard/access/api-keys", "")
	mustContain(t, rec.Body.String(), `data-access-confirm="key-rotate"`)
	mustContain(t, rec.Body.String(), `data-access-confirm="key-revoke"`)
	mustContain(t, rec.Body.String(), `data-key-id="`+key.ID+`"`)

	rec = f.do(t, session, csrf, http.MethodPost, "/api/v1/admin/api-keys/"+key.ID+"/regenerate", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("regenerate: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	mustContain(t, rec.Body.String(), "secret_key")

	rec = f.do(t, session, csrf, http.MethodDelete, "/api/v1/admin/api-keys/"+key.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = f.do(t, session, csrf, http.MethodGet, "/dashboard/access/api-keys", "")
	mustNotContain(t, rec.Body.String(), `data-key-id="`+key.ID+`"`)
}
