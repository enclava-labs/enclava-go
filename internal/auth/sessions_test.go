package auth_test

import (
	"context"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/testdb"
)

// TestPrincipalForUserUnionsRoleGrants proves session principals carry
// exactly the union of their roles' stored grants: All flags OR across
// roles, item sets union, assistants feed both chatbot and agent allowlists,
// and a platform.admin-scoped role without grants contributes zero resource
// authority (D-25: no implicit admin grant).
func TestPrincipalForUserUnionsRoleGrants(t *testing.T) {
	db, repo, svc, tenant := seedEnv(t)
	ctx := context.Background()

	scopedList := auth.Role{
		ID:     "role_union_list",
		Name:   "union-list",
		Scopes: []string{"models.list", "chat"},
		Grants: auth.ResourceGrants{
			Models:    auth.Allowlist{Items: []string{"model_a"}},
			Endpoints: auth.Allowlist{Items: []string{"endpoint_x"}},
		},
	}
	allAssistant := auth.Role{
		ID:     "role_union_all",
		Name:   "union-all",
		Scopes: []string{"chatbot"},
		Grants: auth.ResourceGrants{
			Assistants:       auth.Allowlist{All: true},
			ExtractTemplates: auth.Allowlist{All: true},
		},
	}
	adminNoGrants := auth.Role{
		ID:     "role_union_admin",
		Name:   "union-admin",
		Scopes: []string{auth.ScopePlatformAdmin},
	}
	for _, role := range []auth.Role{scopedList, allAssistant, adminNoGrants} {
		if err := repo.CreateRole(ctx, tenant, role); err != nil {
			t.Fatalf("create role %s: %v", role.ID, err)
		}
	}
	hash, err := auth.HashPassword("union-password")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	user := auth.User{ID: "user_union", TenantID: tenant, Email: "union@example.com", DisplayName: "union", Status: "active", PasswordHash: hash}
	if err := repo.CreateUser(ctx, tenant, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	for _, roleID := range []string{scopedList.ID, allAssistant.ID, adminNoGrants.ID} {
		if err := repo.AssignRole(ctx, tenant, user.ID, roleID); err != nil {
			t.Fatalf("assign %s: %v", roleID, err)
		}
	}

	p, err := svc.AuthenticatePassword(ctx, tenant, "union@example.com", "union-password")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if _, ok := p.AllowedModels["model_a"]; !ok {
		t.Fatalf("model_a missing from union: %v", p.AllowedModels)
	}
	if p.AllowedModelsAll {
		t.Fatalf("list grant must not set All: %+v", p)
	}
	if _, ok := p.AllowedEndpoints["endpoint_x"]; !ok {
		t.Fatalf("endpoint_x missing from union: %v", p.AllowedEndpoints)
	}
	if !p.AllowedChatbotsAll || !p.AllowedAgentsAll {
		t.Fatalf("assistant mode:all must feed both chatbot and agent All flags: %+v", p)
	}
	if !p.AllowedExtractTemplatesAll {
		t.Fatalf("template mode:all lost: %+v", p)
	}
	if !auth.HasScope(p, auth.ScopePlatformAdmin) {
		t.Fatalf("platform.admin scope must still union: %v", p.Scopes)
	}
	// The admin-scoped role carries no grants: nothing beyond the other
	// roles' items, and no IP grants (roles never carry IPs).
	if len(p.AllowedModels) != 1 || len(p.AllowedEndpoints) != 1 {
		t.Fatalf("admin-scoped role leaked resource grants: models=%v endpoints=%v", p.AllowedModels, p.AllowedEndpoints)
	}
	if len(p.AllowedIPs) != 0 || p.AllowedIPsAll {
		t.Fatalf("roles must not grant IPs: %v %v", p.AllowedIPs, p.AllowedIPsAll)
	}

	// PrincipalForActor (the durable-operation path) shows the same union.
	actor, err := svc.PrincipalForActor(ctx, tenant, user.ID)
	if err != nil {
		t.Fatalf("principal for actor: %v", err)
	}
	if _, ok := actor.AllowedModels["model_a"]; !ok || !actor.AllowedExtractTemplatesAll {
		t.Fatalf("actor principal union diverges: %+v", actor)
	}

	// A user whose ONLY role is platform.admin without grants denies every
	// resource — no implicit admin grant anywhere.
	adminOnly := testdb.SeedUser(t, db, tenant, "adminonly@example.com", []string{auth.ScopePlatformAdmin})
	adminPrincipal, err := svc.PrincipalForActor(ctx, tenant, adminOnly.ID)
	if err != nil {
		t.Fatalf("admin-only principal: %v", err)
	}
	if adminPrincipal.AllowedModelsAll || adminPrincipal.AllowedChatbotsAll ||
		adminPrincipal.AllowedAgentsAll || adminPrincipal.AllowedEndpointsAll ||
		adminPrincipal.AllowedExtractTemplatesAll || len(adminPrincipal.AllowedModels) != 0 {
		t.Fatalf("platform.admin implied resource grants: %+v", adminPrincipal)
	}
}

// TestSessionGrantsRefreshOnRoleUpdate proves a role grant edit is visible on
// the member's NEXT request — no re-login (OA-20 runtime-refresh
// precondition).
func TestSessionGrantsRefreshOnRoleUpdate(t *testing.T) {
	db, repo, svc, tenant := seedEnv(t)
	ctx := context.Background()

	role := auth.Role{
		ID:     "role_refresh",
		Name:   "refresh",
		Scopes: []string{"models.list"},
		Grants: auth.ResourceGrants{Models: auth.Allowlist{Items: []string{"model_a"}}},
	}
	if err := repo.CreateRole(ctx, tenant, role); err != nil {
		t.Fatalf("create role: %v", err)
	}
	user := testdb.SeedUser(t, db, tenant, "refresh@example.com", nil)
	if err := repo.AssignRole(ctx, tenant, user.ID, role.ID); err != nil {
		t.Fatalf("assign role: %v", err)
	}

	token, _, _, err := svc.IssueSession(ctx, tenant, user.ID, time.Hour)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	p, err := svc.AuthenticateSession(ctx, token)
	if err != nil {
		t.Fatalf("authenticate session: %v", err)
	}
	if _, ok := p.AllowedModels["model_a"]; !ok || p.AllowedModelsAll {
		t.Fatalf("initial grants wrong: models=%v all=%v", p.AllowedModels, p.AllowedModelsAll)
	}

	// Admin edits the role's grant columns directly (06-03 adds the HTTP
	// surface); the change must apply on the next request.
	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE roles SET allowed_models = '{"mode":"all"}'::jsonb WHERE tenant_id = $1::uuid AND id = $2`,
		tenant, role.ID); err != nil {
		t.Fatalf("update role grants: %v", err)
	}
	p, err = svc.AuthenticateSession(ctx, token)
	if err != nil {
		t.Fatalf("re-authenticate session: %v", err)
	}
	if !p.AllowedModelsAll {
		t.Fatalf("role edit must apply on the next request: %+v", p)
	}

	// Revocation is equally immediate.
	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE roles SET allowed_models = '{"mode":"list","items":[]}'::jsonb WHERE tenant_id = $1::uuid AND id = $2`,
		tenant, role.ID); err != nil {
		t.Fatalf("revoke role grants: %v", err)
	}
	p, err = svc.AuthenticateSession(ctx, token)
	if err != nil {
		t.Fatalf("re-authenticate session: %v", err)
	}
	if p.AllowedModelsAll || len(p.AllowedModels) != 0 {
		t.Fatalf("revoked grant must deny the next request: %+v", p)
	}
}

func TestSessionLifecycleAndRevocation(t *testing.T) {
	db, repo, svc, tenant := seedEnv(t)
	user := testdb.SeedUser(t, db, tenant, "s@example.com", []string{"models.list"})
	ctx := context.Background()

	token, csrf, sessionID, err := svc.IssueSession(ctx, tenant, user.ID, time.Hour)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	if token == "" || csrf == "" || sessionID == "" {
		t.Fatalf("empty token/csrf/id")
	}
	p, err := svc.AuthenticateSession(ctx, token)
	if err != nil {
		t.Fatalf("authenticate session: %v", err)
	}
	if p.TenantID != tenant || p.UserID != user.ID || p.SessionID != sessionID ||
		p.CredentialKind != auth.CredentialKindSession {
		t.Fatalf("principal = %+v", p)
	}
	if !auth.HasScope(p, "models.list") {
		t.Fatalf("role scopes missing: %v", p.Scopes)
	}
	if err := svc.ValidateCSRF(ctx, tenant, sessionID, csrf); err != nil {
		t.Fatalf("issued csrf rejected: %v", err)
	}
	if err := svc.ValidateCSRF(ctx, tenant, sessionID, "forged"); err == nil {
		t.Fatalf("forged csrf accepted")
	}

	if err := svc.RevokeSession(ctx, tenant, sessionID, "test"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := svc.AuthenticateSession(ctx, token); err == nil {
		t.Fatalf("revoked session must deny the next request")
	}
	// RevokeUserSessions denies all remaining sessions.
	token2, _, _, err := svc.IssueSession(ctx, tenant, user.ID, time.Hour)
	if err != nil {
		t.Fatalf("issue second session: %v", err)
	}
	if err := svc.RevokeUserSessions(ctx, tenant, user.ID, "logout-all"); err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	if _, err := svc.AuthenticateSession(ctx, token2); err == nil {
		t.Fatalf("user-revoked session must deny")
	}
	_ = repo
}

func TestSessionAbsoluteAndIdleExpiry(t *testing.T) {
	db, repo, svc, tenant := seedEnv(t)
	user := testdb.SeedUser(t, db, tenant, "s2@example.com", nil)
	ctx := context.Background()

	token, _, sessionID, err := svc.IssueSession(ctx, tenant, user.ID, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// Force absolute expiry.
	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE sessions SET expires_at = now() - interval '1 minute' WHERE tenant_id = $1 AND id = $2`,
		tenant, sessionID); err != nil {
		t.Fatalf("expire session: %v", err)
	}
	if _, err := svc.AuthenticateSession(ctx, token); err == nil {
		t.Fatalf("expired session must deny")
	}

	// Idle expiry: issued fresh, idle window pushed into the past.
	token2, _, sessionID2, err := svc.IssueSession(ctx, tenant, user.ID, time.Hour)
	if err != nil {
		t.Fatalf("issue second: %v", err)
	}
	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE sessions SET idle_expires_at = now() - interval '1 minute' WHERE tenant_id = $1 AND id = $2`,
		tenant, sessionID2); err != nil {
		t.Fatalf("idle-expire session: %v", err)
	}
	if _, err := svc.AuthenticateSession(ctx, token2); err == nil {
		t.Fatalf("idle-expired session must deny")
	}
	_ = repo
}
