// identities_test.go — privilege, ownership, session/key and linking
// regressions for the /api/v1/identities surface (plan 01-24). Every test runs
// against the real migrated schema; there are no mocked repositories.
package httpx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/connectors"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

// newIdentityApp builds a real app over an isolated test schema and returns
// the app, repo and an admin API-key secret (scopes: platform.admin only).
// adminMay also needs a mutable schema handle for direct seeding.
type identityFixture struct {
	app      *App
	repo     *postgres.IdentityRepo
	db       *postgres.DB
	tenant   string
	adminKey string
}

func newIdentityFixture(t *testing.T) *identityFixture {
	t.Helper()
	db := testdb.New(t)
	repo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "bootstrap")
	admin := testdb.SeedUser(t, db, tenant, "admin@example.com", []string{auth.ScopePlatformAdmin})
	key, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{
		Name:        "admin",
		OwnerUserID: admin.ID,
		Scopes:      []string{auth.ScopePlatformAdmin},
	})
	authSvc := auth.NewService(repo, "")
	sessionAuth := NewSessionAuth(authSvc, auth.NewOIDCService(repo, nil), time.Hour, DashboardAuthTemplates(), "bootstrap")
	// Every service the grant matrix touches is real, so inner manage
	// checks execute (the production nil-service guards stay in place and
	// unreachable).
	jobsRepo := postgres.NewJobsRepo(db)
	jobsSvc := jobs.NewService(jobsRepo, 0)
	sourceRepo := postgres.NewSourceRepo(db)
	ragSvc := rag.NewService(postgres.NewRAGRepo(db), nil, nil, "", nil, jobsSvc)
	chatSvc := chatbot.NewService(postgres.NewChatbotRepo(db), postgres.NewPromptsRepo(db), ragSvc, nil)
	promptSvc := prompts.NewService(postgres.NewPromptsRepo(db), chatSvc)
	connectorSvc := connectors.NewService(sourceRepo, jobsSvc, jobsRepo)
	app := NewApp(authSvc, nil, "", nil, repo, nil, ragSvc, promptSvc, chatSvc, connectorSvc, nil, nil, postgres.NewEndpointsRepo(db),
		"sha256", "", "enc_", nil, sessionAuth, 1, false, nil, nil, GateConfig{}, sourceRepo, postgres.NewArtifactRepo(db))
	app.ConfigureMCP(postgres.NewMCPToolsRepo(db), nil)
	return &identityFixture{app: app, repo: repo, db: db, tenant: tenant, adminKey: key}
}

func (f *identityFixture) do(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, r)
	return rec
}

func (f *identityFixture) admin(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, method, path, body, map[string]string{"X-API-Key": f.adminKey})
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %d body %q: %v", rec.Code, rec.Body.String(), err)
	}
	return out
}

// login performs the real browser login for the fixture tenant and returns
// the session + CSRF cookies.
func login(t *testing.T, app *App, email, password string) (session, csrf *http.Cookie) {
	t.Helper()
	form := url.Values{"email": {email}, "password": {password}}
	req := httptest.NewRequest(http.MethodPost, "/dashboard/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login for %s expected 303, got %d body=%s", email, rec.Code, rec.Body.String())
	}
	return sessionCookie(t, rec, sessionCookieName), sessionCookie(t, rec, sessionCSRFCookieName)
}

// sessionDo issues an authenticated session request, adding CSRF for
// mutations when the token is supplied.
func sessionDo(t *testing.T, app *App, method, path, body string, session, csrf *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
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

func setPassword(t *testing.T, repo *postgres.IdentityRepo, tenantID, userID, password string) {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := repo.SetPassword(context.Background(), tenantID, userID, hash, time.Now().UTC()); err != nil {
		t.Fatalf("set password: %v", err)
	}
}

func TestIdentityUserLifecycleIsTenantScoped(t *testing.T) {
	f := newIdentityFixture(t)
	other := testdb.SeedTenant(t, f.db, "other")
	foreign := testdb.SeedUser(t, f.db, other, "foreign@example.com", nil)

	// Create a role the admin actually holds scopes for, then a user in it.
	rec := f.admin(t, http.MethodPost, "/api/v1/identities/roles",
		`{"name":"operator","scopes":["platform.admin"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create role: %d %s", rec.Code, rec.Body.String())
	}
	roleID := decodeJSON(t, rec)["id"].(string)

	rec = f.admin(t, http.MethodPost, "/api/v1/identities/users",
		`{"email":"worker@example.com","display_name":"Worker","password":"pw-123456","role_ids":["`+roleID+`"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user: %d %s", rec.Code, rec.Body.String())
	}
	created := decodeJSON(t, rec)
	userID := created["id"].(string)
	if _, leaked := created["password_hash"]; leaked {
		t.Fatalf("create response leaks password hash: %v", created)
	}
	roles := created["roles"].([]any)
	if len(roles) != 1 {
		t.Fatalf("expected 1 assigned role, got %v", roles)
	}

	// List shows exactly our two tenant users — never the foreign tenant's.
	rec = f.admin(t, http.MethodGet, "/api/v1/identities/users", "")
	users := decodeJSON(t, rec)["users"].([]any)
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d: %s", len(users), rec.Body.String())
	}

	// Cross-tenant reads/writes resolve to 404 inside the caller's tenant.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/identities/users/" + foreign.ID, ""},
		{http.MethodPatch, "/api/v1/identities/users/" + foreign.ID, `{"status":"disabled"}`},
		{http.MethodDelete, "/api/v1/identities/users/" + foreign.ID + "/sessions/ses_x", ""},
	} {
		rec = f.admin(t, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s expected 404, got %d %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}

	// Self-promotion attempt: assigning a platform.admin role to a
	// foreign-tenant id resolves 404 before any write; assigning it to our
	// own user still requires the caller to hold those scopes — the admin
	// key does hold platform.admin, so it succeeds, but a non-admin cannot
	// mint the grant at all.
	rec = f.admin(t, http.MethodPost, "/api/v1/identities/users/"+userID+"/roles/"+roleID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("re-assign same role expected 200, got %d %s", rec.Code, rec.Body.String())
	}

	// Non-admin credentials never reach the admin surface.
	ltdUser := testdb.SeedUser(t, f.db, f.tenant, "ltd@example.com", []string{"chatbot"})
	ltdKey, _ := testdb.SeedAPIKey(t, f.db, f.tenant, testdb.SeedKeySpec{
		Name: "ltd", OwnerUserID: ltdUser.ID, Scopes: []string{"chatbot"},
	})
	rec = f.do(t, http.MethodGet, "/api/v1/identities/users", "", map[string]string{"X-API-Key": ltdKey})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin list expected 403, got %d", rec.Code)
	}
	rec = f.do(t, http.MethodPost, "/api/v1/identities/users",
		`{"email":"x@example.com","password":"pw"}`, map[string]string{"X-API-Key": ltdKey})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin create expected 403, got %d", rec.Code)
	}
}

func TestIdentitySelfServiceAndRevocation(t *testing.T) {
	f := newIdentityFixture(t)
	user := testdb.SeedUser(t, f.db, f.tenant, "self@example.com", []string{"chatbot"})
	setPassword(t, f.repo, f.tenant, user.ID, "self-pass")
	sess, csrf := login(t, f.app, "self@example.com", "self-pass")

	// /me renders the caller's own account — tenant/actor from the session.
	rec := sessionDo(t, f.app, http.MethodGet, "/api/v1/identities/me", "", sess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	me := decodeJSON(t, rec)
	if me["id"] != user.ID || me["email"] != "self@example.com" {
		t.Fatalf("me returned wrong user: %v", me)
	}
	if _, leaked := me["password_hash"]; leaked {
		t.Fatalf("me leaks password hash: %v", me)
	}

	// Sessions list: exactly one, marked current, no token material.
	rec = sessionDo(t, f.app, http.MethodGet, "/api/v1/identities/me/sessions", "", sess, nil)
	sessions := decodeJSON(t, rec)["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %v", sessions)
	}
	s0 := sessions[0].(map[string]any)
	if s0["current"] != true {
		t.Fatalf("expected current flag, got %v", s0)
	}
	if _, leaked := s0["token_hash"]; leaked {
		t.Fatalf("session leaks token hash: %v", s0)
	}
	sessionID := s0["id"].(string)

	// Self password change re-proves the current credential.
	rec = sessionDo(t, f.app, http.MethodPost, "/api/v1/identities/me/password",
		`{"current_password":"wrong","password":"new-pass"}`, sess, csrf)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong current password expected 403, got %d %s", rec.Code, rec.Body.String())
	}
	rec = sessionDo(t, f.app, http.MethodPost, "/api/v1/identities/me/password",
		`{"current_password":"self-pass","password":"new-pass"}`, sess, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("password change expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	if _, err := f.repo.GetUser(context.Background(), f.tenant, user.ID); err != nil {
		t.Fatalf("fetch user: %v", err)
	}
	if _, err := f.app.authSvc.AuthenticatePassword(context.Background(), f.tenant, "self@example.com", "new-pass"); err != nil {
		t.Fatalf("new password must authenticate: %v", err)
	}

	// A second session revoked via /me dies on its very next request.
	sess2, _ := login(t, f.app, "self@example.com", "new-pass")
	rec = sessionDo(t, f.app, http.MethodGet, "/api/v1/identities/me/sessions", "", sess, nil)
	sessions = decodeJSON(t, rec)["sessions"].([]any)
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %v", sessions)
	}
	var otherID string
	for _, s := range sessions {
		m := s.(map[string]any)
		if m["current"] != true {
			otherID = m["id"].(string)
		}
	}
	rec = sessionDo(t, f.app, http.MethodDelete, "/api/v1/identities/me/sessions/"+otherID, "", sess, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke session expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	rec = sessionDo(t, f.app, http.MethodGet, "/api/v1/identities/me", "", sess2, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session must deny next request, got %d", rec.Code)
	}

	// A session-authenticated non-admin still cannot reach admin surfaces.
	rec = sessionDo(t, f.app, http.MethodGet, "/api/v1/identities/users", "", sess, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin session expected 403, got %d", rec.Code)
	}
	_ = sessionID
}

func TestIdentityPasswordChangeRevokesOtherSessions(t *testing.T) {
	f := newIdentityFixture(t)
	audit := postgres.NewSecurityAuditRepo(f.db)
	f.app.SetSecurityAudit(audit)
	user := testdb.SeedUser(t, f.db, f.tenant, "password-sessions@example.com", nil)
	setPassword(t, f.repo, f.tenant, user.ID, "old-password")
	current, csrf := login(t, f.app, "password-sessions@example.com", "old-password")
	other, _ := login(t, f.app, "password-sessions@example.com", "old-password")

	rec := sessionDo(t, f.app, http.MethodPost, "/api/v1/identities/me/password",
		`{"current_password":"wrong","password":"new-password"}`, current, csrf)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong password: %d %s", rec.Code, rec.Body.String())
	}
	rec = sessionDo(t, f.app, http.MethodGet, "/api/v1/identities/me", "", other, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("failed change revoked another session: %d", rec.Code)
	}

	rec = sessionDo(t, f.app, http.MethodPost, "/api/v1/identities/me/password",
		`{"current_password":"old-password","password":"new-password"}`, current, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("change password: %d %s", rec.Code, rec.Body.String())
	}
	rec = sessionDo(t, f.app, http.MethodGet, "/api/v1/identities/me", "", current, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("current session should survive: %d", rec.Code)
	}
	rec = sessionDo(t, f.app, http.MethodGet, "/api/v1/identities/me", "", other, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("other session should be revoked: %d", rec.Code)
	}

	other, _ = login(t, f.app, "password-sessions@example.com", "new-password")
	rec = f.admin(t, http.MethodPost, "/api/v1/identities/users/"+user.ID+"/password", `{"password":"admin-reset"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin reset: %d %s", rec.Code, rec.Body.String())
	}
	for _, sess := range []*http.Cookie{current, other} {
		rec = sessionDo(t, f.app, http.MethodGet, "/api/v1/identities/me", "", sess, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("admin reset should revoke every session: %d", rec.Code)
		}
	}
	rows, err := audit.List(context.Background(), f.tenant, 100, "")
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	counts := map[string]int{}
	for _, row := range rows {
		if row.TargetKind != "user" || row.TargetID != user.ID || row.Outcome != "success" {
			t.Fatalf("unexpected audit row: %+v", row)
		}
		counts[row.Action]++
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"old-password", "new-password", "admin-reset", "wrong"} {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("audit contains password %q", secret)
			}
		}
	}
	if counts["auth.password.changed"] != 2 || counts["auth.sessions.revoked"] != 2 || len(rows) != 4 {
		t.Fatalf("password audit events: %v", counts)
	}
}

func TestIdentityRoleSecurityAudit(t *testing.T) {
	f := newIdentityFixture(t)
	audit := postgres.NewSecurityAuditRepo(f.db)
	f.app.SetSecurityAudit(audit)
	user := testdb.SeedUser(t, f.db, f.tenant, "role-audit@example.com", nil)
	rec := f.admin(t, http.MethodPost, "/api/v1/identities/roles", `{"name":"audited","scopes":[]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create role: %d %s", rec.Code, rec.Body.String())
	}
	roleID := decodeJSON(t, rec)["id"].(string)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPatch, "/api/v1/identities/roles/" + roleID, `{"name":"renamed"}`},
		{http.MethodPut, "/api/v1/identities/users/" + user.ID + "/roles/" + roleID, ""},
		{http.MethodDelete, "/api/v1/identities/users/" + user.ID + "/roles/" + roleID, ""},
		{http.MethodDelete, "/api/v1/identities/roles/" + roleID, ""},
	} {
		rec = f.admin(t, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	rows, err := audit.List(context.Background(), f.tenant, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"identity.role.created": roleID, "identity.role.updated": roleID,
		"identity.role.assigned": user.ID, "identity.role.unassigned": user.ID,
		"identity.role.deleted": roleID,
	}
	for _, row := range rows {
		target, ok := want[row.Action]
		if !ok || row.TargetID != target || row.Outcome != "success" || row.ActorID == "" || row.TenantID != f.tenant {
			t.Fatalf("unexpected role audit row: %+v", row)
		}
		if (row.Action == "identity.role.assigned" || row.Action == "identity.role.unassigned") && row.Details["role_id"] != roleID {
			t.Fatalf("membership audit missing role: %+v", row)
		}
		delete(want, row.Action)
	}
	if len(want) != 0 {
		t.Fatalf("missing role audit events: %v", want)
	}
}

func TestIdentityAdminSessionOpsAndLink(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	// Service-account provisioning honours the caller-scope subset rule.
	rec := f.admin(t, http.MethodPost, "/api/v1/identities/service-accounts",
		`{"name":"bot","scopes":["platform.admin","pricing.manage"]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("superset scopes expected 403, got %d %s", rec.Code, rec.Body.String())
	}
	rec = f.admin(t, http.MethodPost, "/api/v1/identities/service-accounts",
		`{"name":"bot","scopes":["platform.admin"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create service account: %d %s", rec.Code, rec.Body.String())
	}
	saID := decodeJSON(t, rec)["id"].(string)

	// A key issued for the service account authenticates and obeys scopes.
	rec = f.admin(t, http.MethodPost, "/api/v1/admin/api-keys",
		`{"name":"bot-key","service_account_id":"`+saID+`","scopes":["platform.admin"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue sa key: %d %s", rec.Code, rec.Body.String())
	}
	saKey := decodeJSON(t, rec)["secret_key"].(string)
	rec = f.do(t, http.MethodGet, "/api/v1/identities/service-accounts", "",
		map[string]string{"X-API-Key": saKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("sa key auth expected 200, got %d %s", rec.Code, rec.Body.String())
	}

	// Secret material is never rendered on list/read.
	rec = f.admin(t, http.MethodGet, "/api/v1/admin/api-keys", "")
	for _, k := range decodeJSON(t, rec)["api_keys"].([]any) {
		m := k.(map[string]any)
		// The stored record marshals with Go field names; the redaction
		// contract is that KeyHash/KeyHashAlgo are always blank.
		if m["KeyHash"] != "" || m["KeyHashAlgo"] != "" {
			t.Fatalf("key list leaks hash material: %v", m)
		}
	}
	if strings.Contains(rec.Body.String(), saKey) {
		t.Fatalf("key list must never contain the plaintext secret")
	}

	// Disabling the service account denies its key on the next request.
	rec = f.admin(t, http.MethodPatch, "/api/v1/identities/service-accounts/"+saID,
		`{"status":"disabled"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable sa expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	rec = f.do(t, http.MethodGet, "/api/v1/identities/service-accounts", "",
		map[string]string{"X-API-Key": saKey})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled sa key must deny, got %d", rec.Code)
	}

	// Explicit OIDC link: a pending challenge binds only after the caller's
	// own password verifies; identical emails never merge accounts.
	linked := testdb.SeedUser(t, f.db, f.tenant, "linkme@example.com", nil)
	setPassword(t, f.repo, f.tenant, linked.ID, "link-pass")
	provider := auth.OIDCProvider{
		ID: "oidc_test", Name: "Test IdP", Issuer: "https://idp.example.com",
		ClientID: "cid", ClientSecretRef: "ref", RedirectPath: "/cb", Enabled: true,
	}
	if err := f.repo.CreateOIDCProvider(ctx, f.tenant, provider); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	linkToken := "linktok_testvalue"
	sum := sha256.Sum256([]byte(linkToken))
	if err := f.repo.CreateOIDCState(ctx, f.tenant, auth.OIDCState{
		ID:                "olink_test",
		StateHash:         hex.EncodeToString(sum[:]),
		ProviderID:        provider.ID,
		Nonce:             "link",
		PKCEVerifier:      "-",
		Subject:           "subject-1",
		Email:             "linkme@example.com",
		PendingLinkUserID: linked.ID,
		ExpiresAt:         time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed link challenge: %v", err)
	}

	// Wrong password refuses the link and does not consume the challenge.
	lsess, lcsrf := login(t, f.app, "linkme@example.com", "link-pass")
	rec = sessionDo(t, f.app, http.MethodPost, "/api/v1/identities/link",
		`{"link_token":"`+linkToken+`","password":"wrong"}`, lsess, lcsrf)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong-password link expected 403, got %d %s", rec.Code, rec.Body.String())
	}
	links, err := f.repo.ListAuthIdentitiesForUser(ctx, f.tenant, linked.ID)
	if err != nil || len(links) != 0 {
		t.Fatalf("no identity must be recorded on failure, got %v %v", links, err)
	}

	// Correct password binds issuer+subject exactly once; replay denies.
	rec = sessionDo(t, f.app, http.MethodPost, "/api/v1/identities/link",
		`{"link_token":"`+linkToken+`","password":"link-pass"}`, lsess, lcsrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("link expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	links, err = f.repo.ListAuthIdentitiesForUser(ctx, f.tenant, linked.ID)
	if err != nil || len(links) != 1 || links[0].Issuer != provider.Issuer || links[0].Subject != "subject-1" {
		t.Fatalf("expected one recorded link, got %v %v", links, err)
	}
	rec = sessionDo(t, f.app, http.MethodPost, "/api/v1/identities/link",
		`{"link_token":"`+linkToken+`","password":"link-pass"}`, lsess, lcsrf)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("replayed link token expected 403, got %d", rec.Code)
	}

	// /me/identities renders the binding server-side.
	rec = sessionDo(t, f.app, http.MethodGet, "/api/v1/identities/me/identities", "", lsess, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "subject-1") {
		t.Fatalf("identities list expected link, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestLastAdminGuardAndKeyScopeSubset(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	users, _ := f.repo.ListUsers(ctx, f.tenant)
	var adminID string
	for _, u := range users {
		if u.Email == "admin@example.com" {
			adminID = u.ID
		}
	}

	// A service-account-owned admin key gives the tenant a second holder, so
	// disabling the sole admin user cannot lock anyone out.
	sa := testdb.SeedServiceAccount(t, f.db, f.tenant, "break-glass", []string{auth.ScopePlatformAdmin})
	rec := f.admin(t, http.MethodPost, "/api/v1/admin/api-keys",
		`{"name":"break-glass","service_account_id":"`+sa.ID+`","scopes":["platform.admin"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue second admin key: %d %s", rec.Code, rec.Body.String())
	}
	secondKey := decodeJSON(t, rec)["secret_key"].(string)

	rec = f.admin(t, http.MethodPatch, "/api/v1/identities/users/"+adminID, `{"status":"disabled"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable admin expected 200 while admin keys remain, got %d %s", rec.Code, rec.Body.String())
	}

	// Deleting the user-owned key while key 2 still holds platform.admin is
	// allowed; deleting key 2 next would strand the tenant → 409.
	// The disabled owner's key no longer authenticates — key 2 does the
	// remaining administration.
	rec = f.do(t, http.MethodGet, "/api/v1/admin/api-keys", "",
		map[string]string{"X-API-Key": secondKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("list via second key expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	var firstID, secondID string
	for _, k := range decodeJSON(t, rec)["api_keys"].([]any) {
		m := k.(map[string]any)
		if m["OwnerUserID"] == adminID {
			firstID = m["ID"].(string)
		}
		if m["ServiceAccountID"] == sa.ID {
			secondID = m["ID"].(string)
		}
	}
	if firstID == "" || secondID == "" {
		t.Fatalf("expected both admin keys listed, got %s", rec.Body.String())
	}
	rec = f.do(t, http.MethodDelete, "/api/v1/admin/api-keys/"+firstID, "",
		map[string]string{"X-API-Key": secondKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("revoking one of two admin keys expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	rec = f.do(t, http.MethodDelete, "/api/v1/admin/api-keys/"+secondID, "",
		map[string]string{"X-API-Key": secondKey})
	if rec.Code != http.StatusConflict {
		t.Fatalf("revoking last admin credential expected 409, got %d %s", rec.Code, rec.Body.String())
	}

	// Re-enable the user with the surviving admin credential.
	rec = f.do(t, http.MethodPatch, "/api/v1/identities/users/"+adminID, `{"status":"active"}`,
		map[string]string{"X-API-Key": secondKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("re-enable admin expected 200, got %d %s", rec.Code, rec.Body.String())
	}

	// The admin key cannot mint a scope it does not hold.
	rec = f.do(t, http.MethodPost, "/api/v1/admin/api-keys",
		`{"name":"esc","scopes":["platform.admin","pricing.manage"]}`,
		map[string]string{"X-API-Key": secondKey})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("key scope escalation expected 403, got %d %s", rec.Code, rec.Body.String())
	}
	rec = f.do(t, http.MethodPost, "/api/v1/admin/api-keys",
		`{"name":"ok","scopes":["platform.admin"]}`,
		map[string]string{"X-API-Key": secondKey})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create scoped key: %d %s", rec.Code, rec.Body.String())
	}
	second := decodeJSON(t, rec)
	if h := second["api_key"].(map[string]any)["KeyHash"]; h != "" && h != nil {
		t.Fatalf("key hash must be redacted, got %v", h)
	}
	if second["secret_key"] == "" {
		t.Fatalf("one-time secret must be returned on create")
	}
}

// grantDocBytes renders one emitted grant document back to bytes so it can
// round-trip through auth.DecodeAllowlist exactly as a client would.
func grantDocBytes(t *testing.T, doc any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal grant doc %v: %v", doc, err)
	}
	return raw
}

// assertDenyDocs asserts a grants envelope carries the canonical explicit
// deny document on all four kinds — decodes deny, never all.
func assertDenyDocs(t *testing.T, grants any, label string) {
	t.Helper()
	m, ok := grants.(map[string]any)
	if !ok {
		t.Fatalf("%s grants not an object: %v", label, grants)
	}
	for _, kind := range []string{"models", "endpoints", "assistants", "extract_templates"} {
		doc, ok := m[kind]
		if !ok {
			t.Fatalf("%s grants missing %q: %v", label, kind, m)
		}
		items, all := auth.DecodeAllowlist(grantDocBytes(t, doc))
		if all || len(items) != 0 {
			t.Fatalf("%s grants[%s] = %v, want explicit deny document", label, kind, doc)
		}
	}
}

// TestIdentityRoleCreateGrantsBounded proves the canonical role grant
// contract end to end: canonical write→store→read→principal documents,
// deny-by-default, caller-bounded writes on create AND assignment, and real
// consumer enforcement (/v1/models intersection, /api/v1/chatbots list).
func TestIdentityRoleCreateGrantsBounded(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	// (a) mode:all round-trip — the canonical write→store→read→principal
	// path only; consumer proof stays separate below.
	rec := f.admin(t, http.MethodPost, "/api/v1/identities/roles",
		`{"name":"all-roundtrip","scopes":["platform.admin"],"grants":{"models":{"mode":"list","items":["m1"]},"assistants":{"mode":"all"}}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("all-grant role create: %d %s", rec.Code, rec.Body.String())
	}
	created := decodeJSON(t, rec)
	roleID := created["id"].(string)
	wantList := map[string]any{"mode": "list", "items": []any{"m1"}}
	wantAll := map[string]any{"mode": "all"}
	grants := created["grants"].(map[string]any)
	if !reflect.DeepEqual(grants["models"], wantList) || !reflect.DeepEqual(grants["assistants"], wantAll) {
		t.Fatalf("create response grants = %v", grants)
	}
	// The omitted kinds carry explicit deny documents, never nil→all.
	assertDenyDocs(t, map[string]any{
		"models": `{"mode":"list","items":[]}`, "endpoints": grants["endpoints"],
		"assistants": `{"mode":"list","items":[]}`, "extract_templates": grants["extract_templates"],
	}, "create response omitted kinds")
	if _, all := auth.DecodeAllowlist(grantDocBytes(t, grants["endpoints"])); all {
		t.Fatalf("omitted endpoints must store explicit deny, got %v", grants["endpoints"])
	}
	if _, all := auth.DecodeAllowlist(grantDocBytes(t, grants["extract_templates"])); all {
		t.Fatalf("omitted extract_templates must store explicit deny, got %v", grants["extract_templates"])
	}

	rec = f.admin(t, http.MethodGet, "/api/v1/identities/roles/"+roleID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("role GET: %d %s", rec.Code, rec.Body.String())
	}
	echoed := decodeJSON(t, rec)["grants"].(map[string]any)
	if !reflect.DeepEqual(echoed["models"], wantList) || !reflect.DeepEqual(echoed["assistants"], wantAll) {
		t.Fatalf("role GET grants = %v", echoed)
	}

	roundtrip := testdb.SeedUser(t, f.db, f.tenant, "roundtrip@example.com", nil)
	setPassword(t, f.repo, f.tenant, roundtrip.ID, "round-pass")
	rec = f.admin(t, http.MethodPost, "/api/v1/identities/users/"+roundtrip.ID+"/roles/"+roleID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("assign all-grant role: %d %s", rec.Code, rec.Body.String())
	}
	principal, err := f.app.authSvc.AuthenticatePassword(ctx, f.tenant, "roundtrip@example.com", "round-pass")
	if err != nil {
		t.Fatalf("authenticate roundtrip user: %v", err)
	}
	if !auth.ModelAllowed(principal, "m1") || auth.ModelAllowed(principal, "m2") {
		t.Fatalf("stored model grant wrong: m1=%v m2=%v", auth.ModelAllowed(principal, "m1"), auth.ModelAllowed(principal, "m2"))
	}
	if !principal.AllowedChatbotsAll || !principal.AllowedAgentsAll ||
		!auth.ChatbotAllowed(principal, "any-bot") || !auth.AgentAllowed(principal, "any-bot") {
		t.Fatalf("assistants mode:all did not reach the principal: %+v", principal)
	}
	if auth.EndpointAllowed(principal, "ep1") || auth.ExtractTemplateAllowed(principal, "t1") {
		t.Fatalf("absent grant kinds must deny: %+v", principal)
	}

	// (b) LIMITED-CONSUMER proof — nonempty explicit lists on ALL FOUR
	// kinds, enforced through the real consumers. The member's grants must
	// supply BOTH the model alias AND its endpoint: permittedModels
	// intersects endpoint and model grants.
	consumerSess, _ := loginUserWithGrants(t, f, "consumer@example.com",
		[]string{"models.list", "chatbot"},
		auth.ResourceGrants{
			Models:           auth.Allowlist{Items: []string{"m1"}},
			Endpoints:        auth.Allowlist{Items: []string{"ep1"}},
			Assistants:       auth.Allowlist{Items: []string{"bot1"}},
			ExtractTemplates: auth.Allowlist{Items: []string{"t1"}},
		}, "consumer-pass")
	consumer, err := f.repo.GetUserByEmail(ctx, f.tenant, "consumer@example.com")
	if err != nil {
		t.Fatalf("fetch consumer: %v", err)
	}

	endpointsRepo := postgres.NewEndpointsRepo(f.db)
	for _, ep := range []struct{ id, alias string }{{"ep1", "m1"}, {"ep2", "m2"}} {
		if err := endpointsRepo.Create(ctx, inference.Endpoint{
			ID: ep.id, TenantID: f.tenant, Protocol: "openai_compatible",
			BaseOrigin:   "https://" + ep.id + ".upstream.test",
			Capabilities: []string{inference.CapabilityChat, inference.ModelCapabilityPrefix + ep.alias},
			Enabled:      true,
		}); err != nil {
			t.Fatalf("seed endpoint %s: %v", ep.id, err)
		}
	}
	botRepo := postgres.NewChatbotRepo(f.db)
	// Audience "*" is tenant-wide (D-07): the audience term intersects on top
	// of grants, so these fixtures isolate the grants-only visibility this
	// test proves.
	for _, b := range []chatbot.Instance{
		{TenantID: f.tenant, ID: "bot1", ExternalID: "bot1", Name: "Bot One", IsEnabled: true, Audience: []string{"*"}},
		{TenantID: f.tenant, ID: "bot2", ExternalID: "bot2", Name: "Bot Two", IsEnabled: true, Audience: []string{"*"}},
	} {
		if err := botRepo.Create(ctx, b); err != nil {
			t.Fatalf("seed chatbot %s: %v", b.ID, err)
		}
	}

	rec = sessionDo(t, f.app, http.MethodGet, "/v1/models", "", consumerSess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("consumer models list: %d %s", rec.Code, rec.Body.String())
	}
	var modelList struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &modelList); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if len(modelList.Data) != 1 || modelList.Data[0].ID != "m1" {
		t.Fatalf("endpoint+model intersection wrong, got %v", modelList.Data)
	}
	if rec := sessionDo(t, f.app, http.MethodGet, "/v1/models/m2", "", consumerSess, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("ungranted model expected 404, got %d %s", rec.Code, rec.Body.String())
	}

	rec = sessionDo(t, f.app, http.MethodGet, "/api/v1/chatbots", "", consumerSess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("consumer chatbots list: %d %s", rec.Code, rec.Body.String())
	}
	var botList struct {
		Chatbots []chatbot.Instance `json:"chatbots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &botList); err != nil {
		t.Fatalf("decode chatbots: %v", err)
	}
	if len(botList.Chatbots) != 1 || botList.Chatbots[0].ID != "bot1" {
		t.Fatalf("assistant grant filter wrong, got %+v", botList.Chatbots)
	}
	if rec := sessionDo(t, f.app, http.MethodGet, "/api/v1/chatbots/bot2", "", consumerSess, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("ungranted chatbot expected 404, got %d %s", rec.Code, rec.Body.String())
	}

	// REVOCATION on the SAME session: the stored document is the single
	// source — the union is rebuilt per request, no re-login needed.
	if _, err := f.db.SQL.ExecContext(ctx,
		`UPDATE roles SET allowed_models='{"mode":"list","items":[]}'::jsonb
		 WHERE tenant_id = $1::uuid AND id IN (SELECT role_id FROM user_roles WHERE user_id = $2)`,
		f.tenant, consumer.ID); err != nil {
		t.Fatalf("revoke models grant: %v", err)
	}
	rec = sessionDo(t, f.app, http.MethodGet, "/v1/models", "", consumerSess, nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &modelList); err != nil {
		t.Fatalf("decode models after revocation: %v", err)
	}
	if len(modelList.Data) != 0 {
		t.Fatalf("revoked model grant still visible: %v", modelList.Data)
	}

	// (c) LIMITED-caller rejection table through a real role-backed session:
	// an endpoint-denied API-key caller would be rejected by the endpoint
	// prefilter before the handler, so only sessions prove this bound.
	limitedSess, limitedCSRF := loginUserWithGrants(t, f, "grant-admin@example.com",
		[]string{auth.ScopePlatformAdmin},
		auth.ResourceGrants{
			Models:           auth.Allowlist{Items: []string{"m1"}},
			Endpoints:        auth.Allowlist{Items: []string{"ep1"}},
			Assistants:       auth.Allowlist{Items: []string{"bot1"}},
			ExtractTemplates: auth.Allowlist{Items: []string{"t1"}},
		}, "limited-pass")
	postRole := func(body string) *httptest.ResponseRecorder {
		return sessionDo(t, f.app, http.MethodPost, "/api/v1/identities/roles", body, limitedSess, limitedCSRF)
	}
	for _, tc := range []struct{ kind, body string }{
		{"models", `{"name":"over-models","scopes":[],"grants":{"models":{"mode":"list","items":["m2"]}}}`},
		{"endpoints", `{"name":"over-endpoints","scopes":[],"grants":{"endpoints":{"mode":"list","items":["ep2"]}}}`},
		{"assistants", `{"name":"over-assistants","scopes":[],"grants":{"assistants":{"mode":"list","items":["bot2"]}}}`},
		{"extract_templates", `{"name":"over-templates","scopes":[],"grants":{"extract_templates":{"mode":"list","items":["t2"]}}}`},
		{"models mode:all", `{"name":"over-all","scopes":[],"grants":{"models":{"mode":"all"}}}`},
	} {
		rec := postRole(tc.body)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "resource authority") {
			t.Fatalf("out-of-authority %s create: expected 403 resource-authority refusal, got %d %s", tc.kind, rec.Code, rec.Body.String())
		}
	}
	// The positive anchor: exactly the granted items on every kind → 201.
	rec = postRole(`{"name":"in-authority","scopes":[],"grants":{` +
		`"models":{"mode":"list","items":["m1"]},` +
		`"endpoints":{"mode":"list","items":["ep1"]},` +
		`"assistants":{"mode":"list","items":["bot1"]},` +
		`"extract_templates":{"mode":"list","items":["t1"]}}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("in-authority role create: %d %s", rec.Code, rec.Body.String())
	}

	// (d) POST with no grants stores explicit deny — real deny, not display.
	rec = f.admin(t, http.MethodPost, "/api/v1/identities/roles", `{"name":"deny-default","scopes":[]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("deny-default role create: %d %s", rec.Code, rec.Body.String())
	}
	denyRole := decodeJSON(t, rec)
	denyRoleID := denyRole["id"].(string)
	assertDenyDocs(t, denyRole["grants"], "create response")
	rec = f.admin(t, http.MethodGet, "/api/v1/identities/roles/"+denyRoleID, "")
	assertDenyDocs(t, decodeJSON(t, rec)["grants"], "role GET")
	rec = f.admin(t, http.MethodGet, "/api/v1/identities/roles", "")
	for _, ro := range decodeJSON(t, rec)["roles"].([]any) {
		if m := ro.(map[string]any); m["id"] == denyRoleID {
			assertDenyDocs(t, m["grants"], "roles list")
		}
	}

	denied := testdb.SeedUser(t, f.db, f.tenant, "denied@example.com", nil)
	setPassword(t, f.repo, f.tenant, denied.ID, "denied-pass")
	rec = f.admin(t, http.MethodPost, "/api/v1/identities/users/"+denied.ID+"/roles/"+denyRoleID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("assign deny role: %d %s", rec.Code, rec.Body.String())
	}

	// (e) DTO-canonical assertions on EVERY role output.
	rec = f.admin(t, http.MethodGet, "/api/v1/identities/users/"+denied.ID, "")
	userRoles := decodeJSON(t, rec)["roles"].([]any)
	if len(userRoles) != 1 {
		t.Fatalf("users GET roles = %v", userRoles)
	}
	assertDenyDocs(t, userRoles[0].(map[string]any)["grants"], "users GET")
	rec = f.admin(t, http.MethodGet, "/api/v1/identities/users/"+denied.ID+"/roles", "")
	assertDenyDocs(t, decodeJSON(t, rec)["roles"].([]any)[0].(map[string]any)["grants"], "membership roles list")
	deniedSess, _ := login(t, f.app, "denied@example.com", "denied-pass")
	rec = sessionDo(t, f.app, http.MethodGet, "/api/v1/identities/me", "", deniedSess, nil)
	meRoles := decodeJSON(t, rec)["roles"].([]any)
	if len(meRoles) != 1 {
		t.Fatalf("/me roles = %v", meRoles)
	}
	assertDenyDocs(t, meRoles[0].(map[string]any)["grants"], "/me")

	principal, err = f.app.authSvc.AuthenticatePassword(ctx, f.tenant, "denied@example.com", "denied-pass")
	if err != nil {
		t.Fatalf("authenticate denied user: %v", err)
	}
	if auth.ModelAllowed(principal, "m1") || auth.EndpointAllowed(principal, "ep1") ||
		auth.ChatbotAllowed(principal, "bot1") || auth.AgentAllowed(principal, "bot1") ||
		auth.ExtractTemplateAllowed(principal, "t1") {
		t.Fatalf("deny-default role must yield zero resource authority: %+v", principal)
	}

	// (f) assignRoleChecked bound: assigning an existing role can never
	// widen the caller's resource authority — direct membership AND the
	// create-user role_ids path.
	rec = f.admin(t, http.MethodPost, "/api/v1/identities/roles",
		`{"name":"wide-role","scopes":[],"grants":{"models":{"mode":"list","items":["m2"]}}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("wide role create: %d %s", rec.Code, rec.Body.String())
	}
	wideRoleID := decodeJSON(t, rec)["id"].(string)
	rec = sessionDo(t, f.app, http.MethodPost, "/api/v1/identities/users/"+denied.ID+"/roles/"+wideRoleID, "", limitedSess, limitedCSRF)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "resource authority") {
		t.Fatalf("out-of-authority assignment expected 403, got %d %s", rec.Code, rec.Body.String())
	}
	rec = f.admin(t, http.MethodPost, "/api/v1/identities/roles",
		`{"name":"narrow-role","scopes":[],"grants":{"models":{"mode":"list","items":["m1"]}}}`)
	narrowRoleID := decodeJSON(t, rec)["id"].(string)
	rec = sessionDo(t, f.app, http.MethodPost, "/api/v1/identities/users/"+denied.ID+"/roles/"+narrowRoleID, "", limitedSess, limitedCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("in-authority assignment expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	rec = sessionDo(t, f.app, http.MethodPost, "/api/v1/identities/users",
		`{"email":"created@example.com","password":"created-pass","role_ids":["`+wideRoleID+`"]}`, limitedSess, limitedCSRF)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "resource authority") {
		t.Fatalf("create-user role_ids escalation expected 403, got %d %s", rec.Code, rec.Body.String())
	}
}

// --- plan 08-02: role PATCH/DELETE (OA-20) + users.team ---------------------

// revokeAdminKeys revokes every platform.admin key in the fixture tenant and
// strips the fixture admin@example.com membership, so the test-seeded roles
// are the only remaining platform.admin holders — the precondition the
// last-admin regressions need (the fixture key and the fixture user would
// otherwise always be surviving holders).
func revokeAdminKeys(t *testing.T, f *identityFixture) {
	t.Helper()
	keys, err := f.repo.ListAPIKeys(context.Background(), f.tenant)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	for _, k := range keys {
		if keyRecordHasScope(k, auth.ScopePlatformAdmin) {
			if err := f.repo.RevokeAPIKey(context.Background(), f.tenant, k.ID, time.Now().UTC()); err != nil {
				t.Fatalf("revoke admin key %s: %v", k.ID, err)
			}
		}
	}
	if _, err := f.db.SQL.ExecContext(context.Background(),
		`DELETE FROM user_roles ur USING users u
		 WHERE ur.user_id = u.id AND u.tenant_id = $1::uuid AND u.email = $2`,
		f.tenant, "admin@example.com"); err != nil {
		t.Fatalf("strip fixture admin membership: %v", err)
	}
}

// roleIDForUser returns the user's first assigned role id.
func roleIDForUser(t *testing.T, f *identityFixture, userID string) string {
	t.Helper()
	roles, err := f.repo.RolesForUser(context.Background(), f.tenant, userID)
	if err != nil || len(roles) == 0 {
		t.Fatalf("roles for %s: %v (%d roles)", userID, err, len(roles))
	}
	return roles[0].ID
}

// roleHasScope reports whether a decoded scopes list carries the scope
// (httpx-side twin of the repo helper).
func roleHasScope(scopes []string, scope string) bool {
	for _, s := range scopes {
		if s == scope {
			return true
		}
	}
	return false
}

func errorCode(t *testing.T, body string) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	errObj, _ := doc["error"].(map[string]any)
	code, _ := errObj["code"].(string)
	return code
}

// TestIdentityRolePatchMergeDeleteAndBounds proves the PATCH/DELETE role
// contract: merge semantics (absent unchanged, present-including-empty
// replaces), caller-bounded scopes and grants, the platform.admin route gate,
// membered-delete refusal and cross-tenant 404s.
func TestIdentityRolePatchMergeDeleteAndBounds(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	path := "/api/v1/identities/roles/"

	rec := f.admin(t, http.MethodPost, "/api/v1/identities/roles",
		`{"name":"ops","scopes":["platform.admin"],"grants":{"models":{"mode":"list","items":["m1"]},"endpoints":{"mode":"list","items":["ep1"]}}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create role: %d %s", rec.Code, rec.Body.String())
	}
	roleID := decodeJSON(t, rec)["id"].(string)

	getRole := func() map[string]any {
		t.Helper()
		rec := f.admin(t, http.MethodGet, path+roleID, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("role GET: %d %s", rec.Code, rec.Body.String())
		}
		return decodeJSON(t, rec)
	}
	before := getRole()

	// Name-only PATCH: scopes and grants byte-identical, name trimmed.
	rec = f.admin(t, http.MethodPatch, path+roleID, `{"name":"  ops renamed  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("name patch: %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeJSON(t, rec); got["name"] != "ops renamed" {
		t.Fatalf("patched name = %v", got["name"])
	}
	after := getRole()
	if !reflect.DeepEqual(after["grants"], before["grants"]) {
		t.Fatalf("name-only patch changed grants: %v", after["grants"])
	}
	if !reflect.DeepEqual(after["scopes"], before["scopes"]) {
		t.Fatalf("name-only patch changed scopes: %v", after["scopes"])
	}

	// Explicit empty scopes list clears; explicit list restores.
	rec = f.admin(t, http.MethodPatch, path+roleID, `{"scopes":[]}`)
	if rec.Code != http.StatusOK || len(getRole()["scopes"].([]any)) != 0 {
		t.Fatalf("scopes clear: %d %s %v", rec.Code, rec.Body.String(), getRole()["scopes"])
	}
	rec = f.admin(t, http.MethodPatch, path+roleID, `{"scopes":["platform.admin"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("scopes restore: %d %s", rec.Code, rec.Body.String())
	}

	// Explicit empty items clear one grant kind; other kinds stay.
	rec = f.admin(t, http.MethodPatch, path+roleID, `{"grants":{"models":{"mode":"list","items":[]}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("models clear: %d %s", rec.Code, rec.Body.String())
	}
	grants := getRole()["grants"].(map[string]any)
	items, all := auth.DecodeAllowlist(grantDocBytes(t, grants["models"]))
	if all || len(items) != 0 {
		t.Fatalf("models grant not cleared: %v", grants["models"])
	}
	if got, _ := auth.DecodeAllowlist(grantDocBytes(t, grants["endpoints"])); len(got) != 1 {
		t.Fatalf("endpoints grant must survive the models-only clear: %v", grants["endpoints"])
	}

	// A new envelope replaces only the kinds it carries; absent kinds keep
	// their stored grant.
	rec = f.admin(t, http.MethodPatch, path+roleID, `{"grants":{"models":{"mode":"list","items":["m1"]}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("envelope replace: %d %s", rec.Code, rec.Body.String())
	}
	grants = getRole()["grants"].(map[string]any)
	if got, all := auth.DecodeAllowlist(grantDocBytes(t, grants["models"])); all || len(got) != 1 {
		t.Fatalf("models grant = %v", grants["models"])
	}
	if got, all := auth.DecodeAllowlist(grantDocBytes(t, grants["endpoints"])); all || len(got) != 1 {
		t.Fatalf("absent endpoints kind must keep its stored grant: %v", grants["endpoints"])
	}

	// Whitespace-only names are refused.
	rec = f.admin(t, http.MethodPatch, path+roleID, `{"name":"   "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blank name patch: %d %s", rec.Code, rec.Body.String())
	}

	// The platform.admin route gate: a member session cannot PATCH/DELETE.
	memberSess, memberCSRF := loginUserWithGrants(t, f, "gate-member@example.com", []string{"chatbot"}, auth.ResourceGrants{}, "gate-pass")
	rec = sessionDo(t, f.app, http.MethodPatch, path+roleID, `{"name":"nope"}`, memberSess, memberCSRF)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member patch expected 403, got %d %s", rec.Code, rec.Body.String())
	}
	rec = sessionDo(t, f.app, http.MethodDelete, path+roleID, "", memberSess, memberCSRF)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member delete expected 403, got %d %s", rec.Code, rec.Body.String())
	}

	// Caller-bounded PATCH: a limited admin session cannot raise grants or
	// scopes beyond its own authority — not even on its own role.
	limitedSess, limitedCSRF := loginUserWithGrants(t, f, "limited-admin@example.com", []string{auth.ScopePlatformAdmin},
		auth.ResourceGrants{Models: auth.Allowlist{Items: []string{"m9"}}}, "limited-pass")
	rec = sessionDo(t, f.app, http.MethodPost, "/api/v1/identities/roles",
		`{"name":"limited-role","scopes":[],"grants":{"models":{"mode":"list","items":["m9"]}}}`, limitedSess, limitedCSRF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("limited role create: %d %s", rec.Code, rec.Body.String())
	}
	limitedRole := decodeJSON(t, rec)["id"].(string)
	for _, tc := range []struct{ label, body string }{
		{"item beyond caller", `{"grants":{"models":{"mode":"list","items":["m1"]}}}`},
		{"mode:all beyond caller", `{"grants":{"models":{"mode":"all"}}}`},
		{"scope beyond caller", `{"scopes":["pricing.manage"]}`},
	} {
		rec = sessionDo(t, f.app, http.MethodPatch, path+limitedRole, tc.body, limitedSess, limitedCSRF)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s patch expected 403, got %d %s", tc.label, rec.Code, rec.Body.String())
		}
	}

	// DELETE a membered role → 409 role_has_members; the role survives.
	target := testdb.SeedUser(t, f.db, f.tenant, "doomed-member@example.com", nil)
	rec = f.admin(t, http.MethodPut, "/api/v1/identities/users/"+target.ID+"/roles/"+roleID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("assign member: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.admin(t, http.MethodDelete, path+roleID, "")
	if rec.Code != http.StatusConflict || errorCode(t, rec.Body.String()) != "role_has_members" {
		t.Fatalf("membered delete: %d %s", rec.Code, rec.Body.String())
	}
	if got := f.admin(t, http.MethodGet, path+roleID, ""); got.Code != http.StatusOK {
		t.Fatalf("membered role must survive refusal, got %d", got.Code)
	}

	// DELETE an empty role → 200; it and every later look is 404.
	rec = f.admin(t, http.MethodPost, "/api/v1/identities/roles", `{"name":"empty-role","scopes":[]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("empty role create: %d %s", rec.Code, rec.Body.String())
	}
	emptyID := decodeJSON(t, rec)["id"].(string)
	rec = f.admin(t, http.MethodDelete, path+emptyID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty role delete: %d %s", rec.Code, rec.Body.String())
	}
	if deleted := decodeJSON(t, rec); deleted["deleted"] != true || deleted["id"] != emptyID {
		t.Fatalf("delete body = %v", deleted)
	}
	if rec := f.admin(t, http.MethodGet, path+emptyID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted role GET: %d", rec.Code)
	}
	if rec := f.admin(t, http.MethodDelete, path+emptyID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted role DELETE: %d", rec.Code)
	}

	// PUT is not part of the frozen contract.
	if rec := f.admin(t, http.MethodPut, path+roleID, `{}`); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("role PUT: %d", rec.Code)
	}

	// Cross-tenant role ids are indistinguishable 404s on both verbs.
	other := testdb.SeedTenant(t, f.db, "patch-foreign-tenant")
	if err := f.repo.CreateRole(ctx, other, auth.Role{ID: "role_foreign", Name: "foreign", Scopes: []string{auth.ScopePlatformAdmin}}); err != nil {
		t.Fatalf("seed foreign role: %v", err)
	}
	rec = f.admin(t, http.MethodPatch, path+"role_foreign", `{"name":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign role patch: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.admin(t, http.MethodDelete, path+"role_foreign", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign role delete: %d %s", rec.Code, rec.Body.String())
	}
}

// TestRolePatchTakesEffectOnNextRequest proves edits are runtime-effective:
// the member's next principal rebuild carries the new stored union (D-25),
// asserted through AuthenticateSession AND a real allowed read.
func TestRolePatchTakesEffectOnNextRequest(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	endpointsRepo := postgres.NewEndpointsRepo(f.db)
	for _, ep := range []struct{ id, alias string }{{"ep1", "m1"}, {"ep2", "m2"}} {
		if err := endpointsRepo.Create(ctx, inference.Endpoint{
			ID: ep.id, TenantID: f.tenant, Protocol: "openai_compatible",
			BaseOrigin:   "https://" + ep.id + ".upstream.test",
			Capabilities: []string{inference.CapabilityChat, inference.ModelCapabilityPrefix + ep.alias},
			Enabled:      true,
		}); err != nil {
			t.Fatalf("seed endpoint %s: %v", ep.id, err)
		}
	}
	memberSess, _ := loginUserWithGrants(t, f, "rt-member@example.com", []string{"models.list"},
		auth.ResourceGrants{
			Models:    auth.Allowlist{Items: []string{"m1"}},
			Endpoints: auth.Allowlist{Items: []string{"ep1"}},
		}, "rt-pass")
	// The widening PATCHer must hold every scope and grant the patched role
	// carries — f.admin's key is platform.admin-only, so the subset bounds
	// would rightly refuse the role's models.list scope. This admin does.
	patchSess, patchCSRF := loginUserWithGrants(t, f, "rt-admin@example.com",
		[]string{auth.ScopePlatformAdmin, "models.list"},
		auth.ResourceGrants{
			Models:    auth.Allowlist{All: true},
			Endpoints: auth.Allowlist{All: true},
		}, "rt-admin-pass")
	member, err := f.repo.GetUserByEmail(ctx, f.tenant, "rt-member@example.com")
	if err != nil {
		t.Fatalf("fetch member: %v", err)
	}
	roleID := roleIDForUser(t, f, member.ID)

	rec := sessionDo(t, f.app, http.MethodGet, "/v1/models", "", memberSess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("baseline models list: %d %s", rec.Code, rec.Body.String())
	}
	var modelList struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &modelList); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if len(modelList.Data) != 1 || modelList.Data[0].ID != "m1" {
		t.Fatalf("baseline grants wrong: %v", modelList.Data)
	}

	// Admin PATCH widens the role's stored union.
	rec = sessionDo(t, f.app, http.MethodPatch, "/api/v1/identities/roles/"+roleID,
		`{"grants":{"models":{"mode":"list","items":["m1","m2"]},"endpoints":{"mode":"list","items":["ep1","ep2"]}}}`, patchSess, patchCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("grants widen: %d %s", rec.Code, rec.Body.String())
	}
	p, err := f.app.authSvc.AuthenticateSession(ctx, memberSess.Value)
	if err != nil {
		t.Fatalf("rebuild principal: %v", err)
	}
	if !auth.ModelAllowed(p, "m2") || !auth.EndpointAllowed(p, "ep2") {
		t.Fatalf("edited grants must apply on the next request: %+v", p)
	}
	rec = sessionDo(t, f.app, http.MethodGet, "/v1/models", "", memberSess, nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &modelList); err != nil {
		t.Fatalf("decode widened models: %v", err)
	}
	if len(modelList.Data) != 2 {
		t.Fatalf("widened grants not live: %v", modelList.Data)
	}

	// Explicit empty items clear the grant on the next request, too.
	rec = sessionDo(t, f.app, http.MethodPatch, "/api/v1/identities/roles/"+roleID,
		`{"grants":{"models":{"mode":"list","items":[]}}}`, patchSess, patchCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("models clear: %d %s", rec.Code, rec.Body.String())
	}
	p, err = f.app.authSvc.AuthenticateSession(ctx, memberSess.Value)
	if err != nil {
		t.Fatalf("rebuild principal after clear: %v", err)
	}
	if auth.ModelAllowed(p, "m2") {
		t.Fatalf("cleared grant still allowed: %+v", p)
	}
}

// TestRolePatchStripLastAdminRefused proves the role-wide last-admin guard:
// stripping platform.admin from the only granting role is a 409 that persists
// nothing, and the identical edit passes once a second holder exists.
func TestRolePatchStripLastAdminRefused(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	revokeAdminKeys(t, f)

	sess, csrf := loginUserWithGrants(t, f, "sole-admin@example.com", []string{auth.ScopePlatformAdmin}, auth.ResourceGrants{}, "sole-pass")
	sole, err := f.repo.GetUserByEmail(ctx, f.tenant, "sole-admin@example.com")
	if err != nil {
		t.Fatalf("fetch sole admin: %v", err)
	}
	roleID := roleIDForUser(t, f, sole.ID)

	rec := sessionDo(t, f.app, http.MethodPatch, "/api/v1/identities/roles/"+roleID, `{"scopes":[]}`, sess, csrf)
	if rec.Code != http.StatusConflict || errorCode(t, rec.Body.String()) != "last_admin" {
		t.Fatalf("sole-admin strip: %d %s", rec.Code, rec.Body.String())
	}
	got, err := f.repo.GetRole(ctx, f.tenant, roleID)
	if err != nil || !roleHasScope(got.Scopes, auth.ScopePlatformAdmin) {
		t.Fatalf("refused strip must not persist: %+v %v", got, err)
	}

	// A second role carrying platform.admin makes the same edit legal.
	testdb.SeedUser(t, f.db, f.tenant, "sole-admin-2@example.com", []string{auth.ScopePlatformAdmin})
	rec = sessionDo(t, f.app, http.MethodPatch, "/api/v1/identities/roles/"+roleID, `{"scopes":[]}`, sess, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("strip with second holder: %d %s", rec.Code, rec.Body.String())
	}
}

// TestRoleLastAdminConcurrentStripRace is the deterministic two-removal
// regression: two concurrent PATCHes stripping platform.admin from the two
// last admin roles must resolve to exactly one success and one 409
// last_admin, never to a tenant without a usable holder.
func TestRoleLastAdminConcurrentStripRace(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	revokeAdminKeys(t, f)

	sessA, csrfA := loginUserWithGrants(t, f, "race-a@example.com", []string{auth.ScopePlatformAdmin}, auth.ResourceGrants{}, "race-a-pass")
	sessB, csrfB := loginUserWithGrants(t, f, "race-b@example.com", []string{auth.ScopePlatformAdmin}, auth.ResourceGrants{}, "race-b-pass")
	userA, err := f.repo.GetUserByEmail(ctx, f.tenant, "race-a@example.com")
	if err != nil {
		t.Fatalf("fetch A: %v", err)
	}
	userB, err := f.repo.GetUserByEmail(ctx, f.tenant, "race-b@example.com")
	if err != nil {
		t.Fatalf("fetch B: %v", err)
	}
	r1 := roleIDForUser(t, f, userA.ID)
	r2 := roleIDForUser(t, f, userB.ID)

	type outcome struct {
		code int
		body string
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	patch := func(sess, csrf *http.Cookie, roleID string) {
		<-start
		rec := sessionDo(t, f.app, http.MethodPatch, "/api/v1/identities/roles/"+roleID, `{"scopes":[]}`, sess, csrf)
		results <- outcome{rec.Code, rec.Body.String()}
	}
	go patch(sessA, csrfA, r1)
	go patch(sessB, csrfB, r2)
	close(start)

	ok, conflict := 0, 0
	for i := 0; i < 2; i++ {
		o := <-results
		switch {
		case o.code == http.StatusOK:
			ok++
		case o.code == http.StatusConflict && errorCode(t, o.body) == "last_admin":
			conflict++
		default:
			t.Fatalf("unexpected race outcome: %d %s", o.code, o.body)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("race must yield exactly one success and one 409, got %d/%d", ok, conflict)
	}
	users, keys, err := f.repo.AdminHolderCounts(ctx, f.tenant, auth.ScopePlatformAdmin)
	if err != nil {
		t.Fatalf("holder counts: %v", err)
	}
	if len(users)+len(keys) < 1 {
		t.Fatalf("concurrent removals stranded the tenant: users=%v keys=%v", users, keys)
	}
}

// TestRoleDeleteVsAdminStripRace proves concurrent role-DELETE and
// admin-scope-removal cannot both pass the remaining-holder check.
func TestRoleDeleteVsAdminStripRace(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	revokeAdminKeys(t, f)

	sessA, csrfA := loginUserWithGrants(t, f, "delrace-a@example.com", []string{auth.ScopePlatformAdmin}, auth.ResourceGrants{}, "delrace-a-pass")
	// B's retained authority is a wildcard-scope role: HasScope grants it
	// platform.admin, but the holder count matches literal platform.admin
	// roles only, so A's role stays the tenant's sole counted holder and
	// exactly one of the two contested removals can commit — deterministically
	// in either interleaving, with B never losing authority mid-race.
	sessB, csrfB := loginUserWithGrants(t, f, "delrace-b@example.com", []string{"*"}, auth.ResourceGrants{}, "delrace-b-pass")
	userA, err := f.repo.GetUserByEmail(ctx, f.tenant, "delrace-a@example.com")
	if err != nil {
		t.Fatalf("fetch A: %v", err)
	}
	r1 := roleIDForUser(t, f, userA.ID)
	// Memberless second admin role, created through the bounded API.
	rec := sessionDo(t, f.app, http.MethodPost, "/api/v1/identities/roles",
		`{"name":"race-empty","scopes":["platform.admin"]}`, sessB, csrfB)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed empty admin role: %d %s", rec.Code, rec.Body.String())
	}
	r2 := decodeJSON(t, rec)["id"].(string)

	type outcome struct {
		code int
		body string
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	go func() {
		<-start
		rec := sessionDo(t, f.app, http.MethodDelete, "/api/v1/identities/roles/"+r2, "", sessA, csrfA)
		results <- outcome{rec.Code, rec.Body.String()}
	}()
	go func() {
		<-start
		rec := sessionDo(t, f.app, http.MethodPatch, "/api/v1/identities/roles/"+r1, `{"scopes":[]}`, sessB, csrfB)
		results <- outcome{rec.Code, rec.Body.String()}
	}()
	close(start)

	ok, conflict := 0, 0
	for i := 0; i < 2; i++ {
		o := <-results
		switch {
		case o.code == http.StatusOK:
			ok++
		case o.code == http.StatusConflict && errorCode(t, o.body) == "last_admin":
			conflict++
		default:
			t.Fatalf("unexpected race outcome: %d %s", o.code, o.body)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("delete-vs-strip race must yield exactly one success, got %d/%d", ok, conflict)
	}
	users, keys, err := f.repo.AdminHolderCounts(ctx, f.tenant, auth.ScopePlatformAdmin)
	if err != nil {
		t.Fatalf("holder counts: %v", err)
	}
	if len(users)+len(keys) < 1 {
		t.Fatalf("race stranded the tenant: users=%v keys=%v", users, keys)
	}
}

// TestRoleStaleWriteRefusedAndMergeFromStoredRow proves the delayed-write
// interleaving: a name-only PATCH parked past a concurrent admin move either
// re-derives from the commit-time stored row or refuses 409 stale_role — the
// stale scopes are never written back and the moved admin grant stays intact.
func TestRoleStaleWriteRefusedAndMergeFromStoredRow(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	revokeAdminKeys(t, f)

	holder := testdb.SeedUser(t, f.db, f.tenant, "stale-holder@example.com", []string{auth.ScopePlatformAdmin})
	target := roleIDForUser(t, f, holder.ID)
	heirUser := testdb.SeedUser(t, f.db, f.tenant, "stale-heir@example.com", []string{auth.ScopePlatformAdmin})
	heir := roleIDForUser(t, f, heirUser.ID)

	// The delayed write's pre-lock read predates the admin move below.
	read, err := f.repo.GetRole(ctx, f.tenant, target)
	if err != nil {
		t.Fatalf("read role: %v", err)
	}

	// The admin move: grant platform.admin on the heir role, strip it from
	// the target — each an atomic locked write (zero readVersion = blind).
	if _, err := f.repo.UpdateRoleAtomically(ctx, f.tenant, heir,
		postgres.RoleUpdateDeltas{Scopes: new([]string{auth.ScopePlatformAdmin})}, time.Time{}); err != nil {
		t.Fatalf("grant admin on heir: %v", err)
	}
	if _, err := f.repo.UpdateRoleAtomically(ctx, f.tenant, target,
		postgres.RoleUpdateDeltas{Scopes: new([]string{})}, time.Time{}); err != nil {
		t.Fatalf("strip admin from target: %v", err)
	}

	// The parked name-only PATCH replays with the stale readVersion →
	// ErrStaleRole; it must never commit over the moved authority.
	if _, err := f.repo.UpdateRoleAtomically(ctx, f.tenant, target,
		postgres.RoleUpdateDeltas{Name: new("renamed-late")}, read.UpdatedAt); !errors.Is(err, postgres.ErrStaleRole) {
		t.Fatalf("delayed name-only write must refuse as stale, got %v", err)
	}
	got, err := f.repo.GetRole(ctx, f.tenant, target)
	if err != nil {
		t.Fatalf("reread role: %v", err)
	}
	if got.Name == "renamed-late" {
		t.Fatalf("stale write committed: %+v", got)
	}
	if roleHasScope(got.Scopes, auth.ScopePlatformAdmin) {
		t.Fatalf("stale write restored admin scopes: %+v", got)
	}

	// A fresh read commits the name-only merge with scopes byte-identical to
	// the commit-time stored row — the moved admin grant stays intact.
	fresh, err := f.repo.GetRole(ctx, f.tenant, heir)
	if err != nil {
		t.Fatalf("read heir: %v", err)
	}
	renamed, err := f.repo.UpdateRoleAtomically(ctx, f.tenant, heir,
		postgres.RoleUpdateDeltas{Name: new("moved-admin")}, fresh.UpdatedAt)
	if err != nil {
		t.Fatalf("fresh name-only write: %v", err)
	}
	if renamed.Name != "moved-admin" || !reflect.DeepEqual(renamed.Scopes, fresh.Scopes) {
		t.Fatalf("name-only write changed scopes: %+v vs %+v", renamed.Scopes, fresh.Scopes)
	}
	if !roleHasScope(renamed.Scopes, auth.ScopePlatformAdmin) {
		t.Fatalf("moved admin grant lost: %+v", renamed)
	}
	users, keys, err := f.repo.AdminHolderCounts(ctx, f.tenant, auth.ScopePlatformAdmin)
	if err != nil {
		t.Fatalf("holder counts: %v", err)
	}
	if len(users)+len(keys) < 1 {
		t.Fatalf("tenant stranded: users=%v keys=%v", users, keys)
	}

	// The handler maps the sentinels onto their 409 codes.
	for _, tc := range []struct {
		err  error
		code string
	}{
		{postgres.ErrStaleRole, "stale_role"},
		{postgres.ErrLastAdmin, "last_admin"},
		{postgres.ErrRoleHasMembers, "role_has_members"},
	} {
		rec := httptest.NewRecorder()
		f.app.writeRoleWriteError(rec, tc.err, "role_update_failed")
		if rec.Code != http.StatusConflict || errorCode(t, rec.Body.String()) != tc.code {
			t.Fatalf("sentinel %v mapped to %d %s, want 409 %s", tc.err, rec.Code, rec.Body.String(), tc.code)
		}
	}
}

// TestRoleAssignDeleteRaceSafeOutcome proves the atomic delete never leaves a
// membership pointing at a deleted role: the assign-vs-delete race resolves
// to either role+member or neither, never a dangling assignment.
func TestRoleAssignDeleteRaceSafeOutcome(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	member := testdb.SeedUser(t, f.db, f.tenant, "assign-race@example.com", nil)

	for i := 0; i < 6; i++ {
		roleID := fmt.Sprintf("role_race_%d", i)
		role := auth.Role{ID: roleID, Name: "race-" + roleID, Scopes: []string{"chatbot"}, Grants: auth.ResourceGrants{}}
		if err := f.repo.CreateRole(ctx, f.tenant, role); err != nil {
			t.Fatalf("create %s: %v", roleID, err)
		}
		assignErr := make(chan error, 1)
		deleteErr := make(chan error, 1)
		go func() { assignErr <- f.repo.AssignRole(ctx, f.tenant, member.ID, roleID) }()
		go func() { deleteErr <- f.repo.DeleteRoleIfUnused(ctx, f.tenant, roleID) }()
		aErr, dErr := <-assignErr, <-deleteErr

		_, gErr := f.repo.GetRole(ctx, f.tenant, roleID)
		members, mErr := f.repo.RoleUsers(ctx, f.tenant, roleID)
		if mErr != nil {
			t.Fatalf("role users %s: %v", roleID, mErr)
		}
		switch {
		case errors.Is(gErr, auth.ErrNotFound):
			// Delete won: no membership may outlive the role, and the losing
			// assign must have failed the foreign key (safe refusal).
			if len(members) != 0 {
				t.Fatalf("membership outlived deleted role %s", roleID)
			}
			if aErr == nil {
				t.Fatalf("assign onto deleted role %s must fail", roleID)
			}
		case gErr != nil:
			t.Fatalf("get role %s: %v", roleID, gErr)
		default:
			// Assign won: the delete must have refused the membered role.
			if len(members) != 1 {
				t.Fatalf("role %s survived without its membership", roleID)
			}
			if !errors.Is(dErr, postgres.ErrRoleHasMembers) {
				t.Fatalf("delete of membered role %s must refuse, got %v", roleID, dErr)
			}
		}
	}
}

// TestIdentityUserTeamAttribute proves users.team persists through create and
// PATCH, reads back through the repo and every user DTO, allows the empty
// state and enforces the 80-char cap.
func TestIdentityUserTeamAttribute(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	rec := f.admin(t, http.MethodPost, "/api/v1/identities/users",
		`{"email":"teamed@example.com","password":"pw-123456","team":"  platform  "}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create teamed user: %d %s", rec.Code, rec.Body.String())
	}
	created := decodeJSON(t, rec)
	if created["team"] != "platform" {
		t.Fatalf("create response team = %v", created["team"])
	}
	userID := created["id"].(string)
	u, err := f.repo.GetUserByEmail(ctx, f.tenant, "teamed@example.com")
	if err != nil || u.Team != "platform" {
		t.Fatalf("repo team readback: %q %v", u.Team, err)
	}

	patch := func(body string) map[string]any {
		t.Helper()
		rec := f.admin(t, http.MethodPatch, "/api/v1/identities/users/"+userID, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("team patch %s: %d %s", body, rec.Code, rec.Body.String())
		}
		return decodeJSON(t, rec)
	}
	if got := patch(`{"team":"payments"}`); got["team"] != "payments" {
		t.Fatalf("patched team = %v", got["team"])
	}
	if u, err := f.repo.GetUser(ctx, f.tenant, userID); err != nil || u.Team != "payments" {
		t.Fatalf("repo team readback after patch: %q %v", u.Team, err)
	}
	// Explicit empty clears ("No team" is a first-class state); absent leaves
	// the stored value unchanged.
	if got := patch(`{"team":""}`); got["team"] != "" {
		t.Fatalf("cleared team = %v", got["team"])
	}
	if got := patch(`{"display_name":"Renamed"}`); got["team"] != "" {
		t.Fatalf("absent team must be unchanged, got %v", got["team"])
	}
	if got := patch(`{"team":"ops"}`); got["team"] != "ops" {
		t.Fatalf("re-set team = %v", got["team"])
	}
	rec = f.admin(t, http.MethodPatch, "/api/v1/identities/users/"+userID,
		`{"team":"`+strings.Repeat("x", 81)+`"}`)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec.Body.String()) != "invalid_team" {
		t.Fatalf("oversized team: %d %s", rec.Code, rec.Body.String())
	}

	// The list DTO carries the stored team.
	rec = f.admin(t, http.MethodGet, "/api/v1/identities/users", "")
	for _, row := range decodeJSON(t, rec)["users"].([]any) {
		if m := row.(map[string]any); m["id"] == userID && m["team"] != "ops" {
			t.Fatalf("users list team = %v", m["team"])
		}
	}

	// Creating without a team stores the empty state.
	rec = f.admin(t, http.MethodPost, "/api/v1/identities/users",
		`{"email":"noteam@example.com","password":"pw-123456"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create plain user: %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeJSON(t, rec)["team"]; got != "" {
		t.Fatalf("default team = %v", got)
	}
}

// TestIdentityPasswordPolicyEnforcedServerSide proves the shared minimum
// length/whitespace policy (AUTH-I02) rejects weak passwords on every
// password-setting API path — the dashboard's client-side hint is not the
// boundary.
func TestIdentityPasswordPolicyEnforcedServerSide(t *testing.T) {
	f := newIdentityFixture(t)
	weak := []string{`"a"`, `"abcdefg"`, `"        "`}

	// Admin create.
	for _, pw := range weak {
		rec := f.admin(t, http.MethodPost, "/api/v1/identities/users",
			`{"email":"weak@example.com","password":`+pw+`}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("create with %s: %d %s, want 400", pw, rec.Code, rec.Body.String())
		}
	}
	if _, err := f.repo.GetUserByEmail(context.Background(), f.tenant, "weak@example.com"); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("rejected create must leave no user: %v", err)
	}
	rec := f.admin(t, http.MethodPost, "/api/v1/identities/users",
		`{"email":"strong@example.com","password":"abcd1234"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("8+ password must be accepted: %d %s", rec.Code, rec.Body.String())
	}
	userID := decodeJSON(t, rec)["id"].(string)

	// Admin reset.
	for _, pw := range weak {
		rec := f.admin(t, http.MethodPost, "/api/v1/identities/users/"+userID+"/password",
			`{"password":`+pw+`}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("admin reset with %s: %d %s, want 400", pw, rec.Code, rec.Body.String())
		}
	}

	// Self-service change (needs an 8+ current password to pass re-proof).
	sess, csrf := login(t, f.app, "strong@example.com", "abcd1234")
	for _, pw := range weak {
		rec := sessionDo(t, f.app, http.MethodPost, "/api/v1/identities/me/password",
			`{"current_password":"abcd1234","password":`+pw+`}`, sess, csrf)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("self change with %s: %d %s, want 400", pw, rec.Code, rec.Body.String())
		}
	}
	rec = sessionDo(t, f.app, http.MethodPost, "/api/v1/identities/me/password",
		`{"current_password":"abcd1234","password":"new-pass-99"}`, sess, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("self change with 8+: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := f.app.authSvc.AuthenticatePassword(context.Background(), f.tenant, "strong@example.com", "new-pass-99"); err != nil {
		t.Fatalf("new password must authenticate: %v", err)
	}
}

// TestIdentityUserCreateRoleValidationAtomic proves a role_ids list is
// validated before any write: an unknown id refuses the whole request with no
// user left behind, so a retry never meets 'already exists' (ADM-I01).
func TestIdentityUserCreateRoleValidationAtomic(t *testing.T) {
	f := newIdentityFixture(t)
	rec := f.admin(t, http.MethodPost, "/api/v1/identities/roles",
		`{"name":"atomic","scopes":["platform.admin"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create role: %d %s", rec.Code, rec.Body.String())
	}
	roleID := decodeJSON(t, rec)["id"].(string)

	rec = f.admin(t, http.MethodPost, "/api/v1/identities/users",
		`{"email":"atomic@example.com","password":"pw-123456","role_ids":["`+roleID+`","role_missing"]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown role expected 404, got %d %s", rec.Code, rec.Body.String())
	}
	if _, err := f.repo.GetUserByEmail(context.Background(), f.tenant, "atomic@example.com"); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("refused create must leave no user: %v", err)
	}

	rec = f.admin(t, http.MethodPost, "/api/v1/identities/users",
		`{"email":"atomic@example.com","password":"pw-123456","role_ids":["`+roleID+`"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("retry after refused create: %d %s", rec.Code, rec.Body.String())
	}
	roles, err := f.repo.RolesForUser(context.Background(), f.tenant, decodeJSON(t, rec)["id"].(string))
	if err != nil || len(roles) != 1 || roles[0].ID != roleID {
		t.Fatalf("membership missing after atomic create: %v %v", roles, err)
	}
}

// TestIdentityCreateResponsesCarryCreatedAt proves create-user and
// create-role payloads carry the persisted created_at — not the year-one
// zero value (ADM-I02).
func TestIdentityCreateResponsesCarryCreatedAt(t *testing.T) {
	f := newIdentityFixture(t)

	rec := f.admin(t, http.MethodPost, "/api/v1/identities/roles",
		`{"name":"stamped","scopes":["platform.admin"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create role: %d %s", rec.Code, rec.Body.String())
	}
	role := decodeJSON(t, rec)
	raw, _ := role["created_at"].(string)
	roleAt, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || roleAt.IsZero() {
		t.Fatalf("role created_at = %q (%v), want persisted timestamp", raw, err)
	}
	storedRole, err := f.repo.GetRole(context.Background(), f.tenant, role["id"].(string))
	if err != nil {
		t.Fatalf("fetch role: %v", err)
	}
	if !roleAt.Equal(storedRole.CreatedAt) {
		t.Fatalf("role created_at %v != stored %v", roleAt, storedRole.CreatedAt)
	}

	rec = f.admin(t, http.MethodPost, "/api/v1/identities/users",
		`{"email":"stamped@example.com","password":"pw-123456","role_ids":["`+role["id"].(string)+`"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user: %d %s", rec.Code, rec.Body.String())
	}
	user := decodeJSON(t, rec)
	raw, _ = user["created_at"].(string)
	userAt, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || userAt.IsZero() {
		t.Fatalf("user created_at = %q (%v), want persisted timestamp", raw, err)
	}
	storedUser, err := f.repo.GetUserByEmail(context.Background(), f.tenant, "stamped@example.com")
	if err != nil {
		t.Fatalf("fetch user: %v", err)
	}
	if !userAt.Equal(storedUser.CreatedAt) {
		t.Fatalf("user created_at %v != stored %v", userAt, storedUser.CreatedAt)
	}
}
