package auth_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestParseAPIKey(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer abc123")
	if got := auth.ParseAPIKey(r); got != "abc123" {
		t.Fatalf("expected abc123, got %s", got)
	}
}

func seedEnv(t *testing.T) (*postgres.DB, *postgres.IdentityRepo, *auth.Service, string) {
	t.Helper()
	db := testdb.New(t)
	repo := postgres.NewIdentityRepo(db)
	return db, repo, auth.NewService(repo, ""), testdb.SeedTenant(t, db, "tenant-"+randName())
}
func randName() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func TestAuthenticateHappyPath(t *testing.T) {
	db, _, svc, tenant := seedEnv(t)
	user := testdb.SeedUser(t, db, tenant, "u@example.com", []string{"chat.completions"})
	key, rec := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{
		Name:        "main",
		OwnerUserID: user.ID,
		Scopes:      []string{"chat.completions", "models.list"},
	})

	p, err := svc.Authenticate(context.Background(), key)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if p.TenantID != tenant {
		t.Fatalf("tenant = %q, want %q", p.TenantID, tenant)
	}
	if p.UserID != user.ID || p.ActorID != user.ID || p.ActorKind != auth.ActorKindUser {
		t.Fatalf("actor = %q/%q kind %v", p.ActorID, p.UserID, p.ActorKind)
	}
	if p.CredentialID != rec.ID || p.CredentialKind != auth.CredentialKindAPIKey {
		t.Fatalf("credential = %q kind %v", p.CredentialID, p.CredentialKind)
	}
	if !auth.HasScope(p, "chat.completions") || !auth.HasScope(p, "models.list") {
		t.Fatalf("scopes missing: %v", p.Scopes)
	}
	if p.BillingSubjectID != user.ID || p.BillingSubjectKind != auth.BillingSubjectUser {
		t.Fatalf("billing subject = %q kind %q", p.BillingSubjectID, p.BillingSubjectKind)
	}
}

func TestAuthenticateWrongSecretDenied(t *testing.T) {
	db, _, svc, tenant := seedEnv(t)
	user := testdb.SeedUser(t, db, tenant, "u@example.com", nil)
	key, rec := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "k", OwnerUserID: user.ID, Scopes: []string{"models.list"}})
	_ = rec
	if _, err := svc.Authenticate(context.Background(), key+"tampered"); err == nil {
		t.Fatalf("expected wrong secret to be denied")
	}
	if _, err := svc.Authenticate(context.Background(), ""); err == nil {
		t.Fatalf("expected missing key to be denied")
	}
}

func TestRevokedKeyDeniesImmediately(t *testing.T) {
	db, repo, svc, tenant := seedEnv(t)
	user := testdb.SeedUser(t, db, tenant, "u@example.com", nil)
	key, rec := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "k", OwnerUserID: user.ID, Scopes: []string{"models.list"}})
	if _, err := svc.Authenticate(context.Background(), key); err != nil {
		t.Fatalf("pre-revocation auth: %v", err)
	}
	if err := repo.RevokeAPIKey(context.Background(), tenant, rec.ID, time.Now().UTC()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), key); err == nil {
		t.Fatalf("revoked key must deny the very next request")
	}
}

func TestExpiredKeyDenied(t *testing.T) {
	db, _, svc, tenant := seedEnv(t)
	user := testdb.SeedUser(t, db, tenant, "u@example.com", nil)
	past := time.Now().Add(-time.Hour).UTC()
	key, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "k", OwnerUserID: user.ID, Scopes: []string{"models.list"}, ExpiresAt: &past})
	if _, err := svc.Authenticate(context.Background(), key); err == nil {
		t.Fatalf("expired key must be denied")
	}
}

func TestTenantResolvedFromStoredRowOnly(t *testing.T) {
	db, _, svc, tenantA := seedEnv(t)
	tenantB := testdb.SeedTenant(t, db, "tenant-b")
	userA := testdb.SeedUser(t, db, tenantA, "a@example.com", nil)
	userB := testdb.SeedUser(t, db, tenantB, "b@example.com", nil)
	key, _ := testdb.SeedAPIKey(t, db, tenantA, testdb.SeedKeySpec{Name: "k", OwnerUserID: userA.ID, Scopes: []string{"models.list"}})

	p, err := svc.Authenticate(context.Background(), key)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if p.TenantID != tenantA {
		t.Fatalf("tenant-A key produced tenant %q", p.TenantID)
	}
	// A tenant-A-scoped identity lookup cannot return a B user.
	repo := postgres.NewIdentityRepo(db)
	if _, err := repo.GetUser(context.Background(), tenantA, userB.ID); err == nil {
		t.Fatalf("tenant-A lookup returned tenant-B user")
	}
}

func TestDisabledUserKeysAndSessionsDeny(t *testing.T) {
	db, repo, svc, tenant := seedEnv(t)
	user := testdb.SeedUser(t, db, tenant, "u@example.com", nil)
	key, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "k", OwnerUserID: user.ID, Scopes: []string{"models.list"}})
	if err := repo.UpdateUserStatus(context.Background(), tenant, user.ID, "disabled"); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), key); err == nil {
		t.Fatalf("disabled user's key must be denied")
	}
}

func TestMalformedAllowlistDenies(t *testing.T) {
	db, _, svc, tenant := seedEnv(t)
	user := testdb.SeedUser(t, db, tenant, "u@example.com", nil)
	key, rec := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "k", OwnerUserID: user.ID, Scopes: []string{"models.list"}})
	if _, err := db.SQL.ExecContext(context.Background(),
		`UPDATE api_keys SET allowed_models = '{"mode":"corrupt"}'::jsonb WHERE tenant_id = $1 AND id = $2`, tenant, rec.ID); err != nil {
		t.Fatalf("corrupt allowlist: %v", err)
	}
	p, err := svc.Authenticate(context.Background(), key)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if p.AllowedModelsAll || len(p.AllowedModels) != 0 || auth.ModelAllowed(p, "m") {
		t.Fatalf("malformed allowlist must deny everything, got all=%v items=%v", p.AllowedModelsAll, p.AllowedModels)
	}
}

func TestAllowlistDecodersAllSixColumns(t *testing.T) {
	db, _, svc, tenant := seedEnv(t)
	user := testdb.SeedUser(t, db, tenant, "u@example.com", nil)
	columns := []struct {
		col string
		get func(auth.Principal) (map[string]struct{}, bool)
	}{
		{"allowed_models", func(p auth.Principal) (map[string]struct{}, bool) { return p.AllowedModels, p.AllowedModelsAll }},
		{"allowed_extract_templates", func(p auth.Principal) (map[string]struct{}, bool) {
			return p.AllowedExtractTemplates, p.AllowedExtractTemplatesAll
		}},
		{"allowed_chatbots", func(p auth.Principal) (map[string]struct{}, bool) { return p.AllowedChatbots, p.AllowedChatbotsAll }},
		{"allowed_agents", func(p auth.Principal) (map[string]struct{}, bool) { return p.AllowedAgents, p.AllowedAgentsAll }},
		{"allowed_endpoints", func(p auth.Principal) (map[string]struct{}, bool) { return p.AllowedEndpoints, p.AllowedEndpointsAll }},
		{"allowed_ips", func(p auth.Principal) (map[string]struct{}, bool) { return p.AllowedIPs, p.AllowedIPsAll }},
	}
	for _, tc := range columns {
		for _, stored := range []struct {
			name    string
			raw     string
			wantAll bool
			wantLen int
		}{
			{"explicit all", `{"mode":"all"}`, true, 0},
			{"list", `{"mode":"list","items":["x","y"]}`, false, 2},
			{"empty list", `{"mode":"list","items":[]}`, false, 0},
			{"malformed", `{"mode":"bogus"}`, false, 0},
		} {
			key, rec := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: tc.col, OwnerUserID: user.ID, Scopes: []string{"models.list"}})
			if _, err := db.SQL.ExecContext(context.Background(),
				`UPDATE api_keys SET `+tc.col+` = $1::jsonb WHERE tenant_id = $2 AND id = $3`, stored.raw, tenant, rec.ID); err != nil {
				t.Fatalf("set %s %s: %v", tc.col, stored.name, err)
			}
			p, err := svc.Authenticate(context.Background(), key)
			if err != nil {
				t.Fatalf("%s %s authenticate: %v", tc.col, stored.name, err)
			}
			items, all := tc.get(p)
			if all != stored.wantAll || len(items) != stored.wantLen {
				t.Fatalf("%s %s: all=%v items=%v", tc.col, stored.name, all, items)
			}
		}
	}
}

func TestDelegation(t *testing.T) {
	db, repo, svc, tenant := seedEnv(t)
	delegator := testdb.SeedUser(t, db, tenant, "d@example.com", []string{"rag.search", "rag.manage"})
	sa := testdb.SeedServiceAccount(t, db, tenant, "delegate", []string{"rag.search", "rag.manage"})
	key, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "sa-key", ServiceAccountID: sa.ID, Scopes: []string{"rag.search"}})
	ctx := context.Background()

	allowed, _ := json.Marshal(map[string]any{"mode": "list", "items": []string{"rag.search"}})
	d := auth.Delegation{
		ID:                       "del_1",
		TenantID:                 tenant,
		DelegatorUserID:          delegator.ID,
		DelegateServiceAccountID: sa.ID,
		AllowedScopes:            allowed,
		ExpiresAt:                time.Now().Add(time.Hour).UTC(),
	}
	if err := repo.CreateDelegation(ctx, tenant, d); err != nil {
		t.Fatalf("create delegation: %v", err)
	}
	delegate, err := svc.Authenticate(ctx, key)
	if err != nil {
		t.Fatalf("authenticate delegate: %v", err)
	}
	p, err := svc.ValidateDelegation(ctx, delegate, delegator.ID)
	if err != nil {
		t.Fatalf("validate delegation: %v", err)
	}
	if p.DelegatedByActorID != delegator.ID {
		t.Fatalf("DelegatedByActorID = %q", p.DelegatedByActorID)
	}
	if p.BillingSubjectID != delegator.ID || p.BillingSubjectKind != auth.BillingSubjectUser {
		t.Fatalf("billing = %q/%q", p.BillingSubjectID, p.BillingSubjectKind)
	}
	if !auth.HasScope(p, "rag.search") || auth.HasScope(p, "rag.manage") {
		t.Fatalf("effective scopes wrong: %v", p.Scopes)
	}

	// Scope widening rejected: allowed_scopes beyond delegator's scopes.
	wide, _ := json.Marshal(map[string]any{"mode": "list", "items": []string{"platform.admin"}})
	d2 := d
	d2.ID = "del_2"
	d2.AllowedScopes = wide
	if err := repo.CreateDelegation(ctx, tenant, d2); err != nil {
		t.Fatalf("create wide delegation: %v", err)
	}
	if _, err := svc.ValidateDelegation(ctx, delegate, delegator.ID); err == nil {
		t.Fatalf("delegation wider than delegator scopes must be rejected")
	}
}

func TestBcryptPasswords(t *testing.T) {
	hash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !auth.VerifyPassword(hash, "correct horse battery staple") {
		t.Fatalf("round-trip verify failed")
	}
	if auth.VerifyPassword(hash, "wrong password") {
		t.Fatalf("wrong password verified")
	}

	_, repo, svc, tenant := seedEnv(t)
	hash2, _ := auth.HashPassword("s3cret")
	now := time.Now().UTC()
	u := auth.User{ID: "user_pw", TenantID: tenant, Email: "pw@example.com", Status: "active", PasswordHash: hash2, PasswordSetAt: &now}
	if err := repo.CreateUser(context.Background(), tenant, u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := svc.AuthenticatePassword(context.Background(), tenant, "pw@example.com", "s3cret"); err != nil {
		t.Fatalf("password auth: %v", err)
	}
	if _, err := svc.AuthenticatePassword(context.Background(), tenant, "pw@example.com", "nope"); err == nil {
		t.Fatalf("wrong password must fail")
	}
}

func TestPrincipalAllowlists(t *testing.T) {
	set := func(items ...string) map[string]struct{} {
		m := make(map[string]struct{}, len(items))
		for _, i := range items {
			m[i] = struct{}{}
		}
		return m
	}
	allPrincipal := auth.Principal{
		AllowedModelsAll:           true,
		AllowedExtractTemplatesAll: true,
		AllowedChatbotsAll:         true,
		AllowedAgentsAll:           true,
		AllowedEndpointsAll:        true,
		AllowedIPsAll:              true,
	}
	if !auth.ModelAllowed(allPrincipal, "any-model") ||
		!auth.ExtractTemplateAllowed(allPrincipal, "any-template") ||
		!auth.ChatbotAllowed(allPrincipal, "any-bot") ||
		!auth.AgentAllowed(allPrincipal, "any-agent") ||
		!auth.EndpointAllowed(allPrincipal, "any-endpoint") ||
		!auth.IPAllowed(allPrincipal, "203.0.113.7") {
		t.Fatalf("expected all-mode grant to allow every resource")
	}
	listPrincipal := auth.Principal{
		AllowedModels:           set("model-a"),
		AllowedExtractTemplates: set("tpl-a"),
		AllowedChatbots:         set("bot-a"),
		AllowedAgents:           set("agent-a"),
		AllowedEndpoints:        set("ep-a"),
		AllowedIPs:              set("198.51.100.1"),
	}
	if !auth.ModelAllowed(listPrincipal, "model-a") ||
		!auth.ExtractTemplateAllowed(listPrincipal, "tpl-a") ||
		!auth.ChatbotAllowed(listPrincipal, "bot-a") ||
		!auth.AgentAllowed(listPrincipal, "agent-a") ||
		!auth.EndpointAllowed(listPrincipal, "ep-a") ||
		!auth.IPAllowed(listPrincipal, "198.51.100.1") {
		t.Fatalf("expected listed values to be allowed")
	}
	if auth.ModelAllowed(listPrincipal, "model-b") ||
		auth.ExtractTemplateAllowed(listPrincipal, "tpl-b") ||
		auth.ChatbotAllowed(listPrincipal, "bot-b") ||
		auth.AgentAllowed(listPrincipal, "agent-b") ||
		auth.EndpointAllowed(listPrincipal, "ep-b") ||
		auth.IPAllowed(listPrincipal, "198.51.100.2") {
		t.Fatalf("expected unlisted values to be denied")
	}
	deny := auth.Principal{}
	if auth.ModelAllowed(deny, "model-a") ||
		auth.ExtractTemplateAllowed(deny, "tpl-a") ||
		auth.ChatbotAllowed(deny, "bot-a") ||
		auth.AgentAllowed(deny, "agent-a") ||
		auth.EndpointAllowed(deny, "ep-a") ||
		auth.IPAllowed(deny, "198.51.100.1") {
		t.Fatalf("expected empty-list grant to deny every resource")
	}
	if items, all := auth.DecodeAllowlist(json.RawMessage(`{"mode":"all"}`)); !all || len(items) != 0 {
		t.Fatalf("mode:all must set all=true with empty items, got all=%v items=%v", all, items)
	}
	if items, all := auth.DecodeAllowlist(json.RawMessage(`{"mode":"list","items":["a","b"]}`)); all || len(items) != 2 {
		t.Fatalf("mode:list must populate items with all=false, got all=%v items=%v", all, items)
	}
	if items, all := auth.DecodeAllowlist(nil); all || len(items) != 0 {
		t.Fatalf("absent grant must deny, got all=%v items=%v", all, items)
	}
	if items, all := auth.DecodeAllowlist(json.RawMessage(`{not json`)); all || len(items) != 0 {
		t.Fatalf("malformed grant must deny, got all=%v items=%v", all, items)
	}
}
