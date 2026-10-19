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
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
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
	sessionAuth := NewSessionAuth(authSvc, auth.NewOIDCService(repo, nil), time.Hour, dashboardTemplates, "bootstrap")
	app := NewApp(authSvc, nil, "", nil, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		"sha256", "", "enc_", nil, sessionAuth, 1, false, nil, nil)
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
