// identities_test.go — privilege, ownership, session/key and linking
// regressions for the /api/v1/identities surface (plan 01-24). Every test runs
// against the real migrated schema; there are no mocked repositories.
package httpx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	for _, b := range []chatbot.Instance{
		{TenantID: f.tenant, ID: "bot1", ExternalID: "bot1", Name: "Bot One", IsEnabled: true},
		{TenantID: f.tenant, ID: "bot2", ExternalID: "bot2", Name: "Bot Two", IsEnabled: true},
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
