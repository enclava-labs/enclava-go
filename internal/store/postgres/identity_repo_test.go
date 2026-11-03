package postgres_test

import (
	"context"
	"errors"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func containsItem(items []string, id string) bool {
	for _, item := range items {
		if item == id {
			return true
		}
	}
	return false
}

// TestRoleGrantsRoundTrip proves mode:all and mode:list grant documents
// survive CreateRole → GetRole/ListRoles/RolesForUser unchanged, and that a
// zero-grants role stores the explicit deny document (deny by default).
func TestRoleGrantsRoundTrip(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	repo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "role-grants")

	all := auth.Role{
		ID:     "role_grants_all",
		Name:   "grants-all",
		Scopes: []string{"platform.admin"},
		Grants: auth.ResourceGrants{
			Models:           auth.Allowlist{All: true},
			Endpoints:        auth.Allowlist{All: true},
			Assistants:       auth.Allowlist{All: true},
			ExtractTemplates: auth.Allowlist{All: true},
		},
	}
	listed := auth.Role{
		ID:     "role_grants_list",
		Name:   "grants-list",
		Scopes: []string{"models.list"},
		Grants: auth.ResourceGrants{
			Models:           auth.Allowlist{Items: []string{"model_a", "model_b"}},
			Endpoints:        auth.Allowlist{Items: []string{"chat/completions"}},
			Assistants:       auth.Allowlist{Items: []string{"bot_1"}},
			ExtractTemplates: auth.Allowlist{Items: []string{"tmpl_1"}},
		},
	}
	denied := auth.Role{ID: "role_grants_deny", Name: "grants-deny", Scopes: []string{"chat"}}

	for _, role := range []auth.Role{all, listed, denied} {
		if err := repo.CreateRole(ctx, tenant, role); err != nil {
			t.Fatalf("create role %s: %v", role.ID, err)
		}
	}

	// GetRole returns each stored document.
	gotAll, err := repo.GetRole(ctx, tenant, all.ID)
	if err != nil {
		t.Fatalf("get all role: %v", err)
	}
	if !gotAll.Grants.Models.All || !gotAll.Grants.Endpoints.All ||
		!gotAll.Grants.Assistants.All || !gotAll.Grants.ExtractTemplates.All {
		t.Fatalf("mode:all grants lost: %+v", gotAll.Grants)
	}

	gotList, err := repo.GetRole(ctx, tenant, listed.ID)
	if err != nil {
		t.Fatalf("get list role: %v", err)
	}
	if gotList.Grants.Models.All || gotList.Grants.Assistants.All {
		t.Fatalf("list role must not decode as all: %+v", gotList.Grants)
	}
	for _, id := range []string{"model_a", "model_b"} {
		if !containsItem(gotList.Grants.Models.Items, id) {
			t.Fatalf("model %s missing from stored items: %v", id, gotList.Grants.Models.Items)
		}
	}
	if len(gotList.Grants.Endpoints.Items) != 1 || len(gotList.Grants.Assistants.Items) != 1 ||
		len(gotList.Grants.ExtractTemplates.Items) != 1 {
		t.Fatalf("item sets lost in round-trip: %+v", gotList.Grants)
	}

	// ListRoles returns the same documents.
	roles, err := repo.ListRoles(ctx, tenant)
	if err != nil || len(roles) != 3 {
		t.Fatalf("list roles: %v %#v", err, roles)
	}
	byID := map[string]auth.Role{}
	for _, role := range roles {
		byID[role.ID] = role
	}
	if !byID[all.ID].Grants.ExtractTemplates.All {
		t.Fatalf("list roles lost all-grant: %+v", byID[all.ID].Grants)
	}
	if !containsItem(byID[listed.ID].Grants.Assistants.Items, "bot_1") {
		t.Fatalf("list roles lost assistant item: %+v", byID[listed.ID].Grants)
	}

	// RolesForUser (the session path) returns the assigned documents.
	user := testdb.SeedUser(t, db, tenant, "grants@example.com", nil)
	if err := repo.AssignRole(ctx, tenant, user.ID, listed.ID); err != nil {
		t.Fatalf("assign role: %v", err)
	}
	userRoles, err := repo.RolesForUser(ctx, tenant, user.ID)
	if err != nil || len(userRoles) != 1 {
		t.Fatalf("roles for user: %v %#v", err, userRoles)
	}
	if !containsItem(userRoles[0].Grants.Models.Items, "model_a") {
		t.Fatalf("session path lost model grant: %+v", userRoles[0].Grants)
	}
	if userRoles[0].Grants.Models.All {
		t.Fatalf("list grant decoded as all: %+v", userRoles[0].Grants)
	}
}

// TestRoleGrantsDenyByDefault proves absent and malformed stored grant
// documents decode as deny — never silently unrestricted.
func TestRoleGrantsDenyByDefault(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	repo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "role-deny")

	// Zero-value grants store the explicit deny document.
	role := auth.Role{ID: "role_deny_doc", Name: "deny-doc", Scopes: []string{"chat"}}
	if err := repo.CreateRole(ctx, tenant, role); err != nil {
		t.Fatalf("create role: %v", err)
	}
	got, err := repo.GetRole(ctx, tenant, role.ID)
	if err != nil {
		t.Fatalf("get role: %v", err)
	}
	for name, a := range map[string]auth.Allowlist{
		"models": got.Grants.Models, "endpoints": got.Grants.Endpoints,
		"assistants": got.Grants.Assistants, "templates": got.Grants.ExtractTemplates,
	} {
		if a.All || len(a.Items) != 0 {
			t.Fatalf("fresh role %s grant = %+v, want deny", name, a)
		}
	}

	// A raw row written without grant columns (column default) and malformed
	// documents both deny.
	if _, err := db.SQL.ExecContext(ctx,
		`INSERT INTO roles (tenant_id, id, name, scopes)
		 VALUES ($1::uuid, 'role_deny_default', 'deny-default', '[]'::jsonb)`,
		tenant); err != nil {
		t.Fatalf("seed raw role: %v", err)
	}
	got, err = repo.GetRole(ctx, tenant, "role_deny_default")
	if err != nil {
		t.Fatalf("get raw role: %v", err)
	}
	if got.Grants.Models.All || len(got.Grants.Models.Items) != 0 {
		t.Fatalf("column default must deny, got %+v", got.Grants.Models)
	}

	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE roles
		   SET allowed_models = '{"mode":"whatever"}'::jsonb,
		       allowed_endpoints = 'null'::jsonb
		 WHERE tenant_id = $1::uuid AND id = 'role_deny_default'`,
		tenant); err != nil {
		t.Fatalf("corrupt grants: %v", err)
	}
	got, err = repo.GetRole(ctx, tenant, "role_deny_default")
	if err != nil {
		t.Fatalf("get corrupted role: %v", err)
	}
	for name, a := range map[string]auth.Allowlist{
		"models": got.Grants.Models, "endpoints": got.Grants.Endpoints,
	} {
		if a.All || len(a.Items) != 0 {
			t.Fatalf("malformed %s grant must deny, got %+v", name, a)
		}
	}
}

// TestCreateUserWithRolesAtomic proves the user row and its initial
// memberships commit or refuse together: an unknown role id returns
// auth.ErrNotFound and leaves no user behind (ADM-I01), while all-valid roles
// persist alongside the stored created_at (ADM-I02).
func TestCreateUserWithRolesAtomic(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	repo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "atomic-create")

	roleA := auth.Role{ID: "role_atomic_a", Name: "atomic-a", Scopes: []string{"chat"}}
	roleB := auth.Role{ID: "role_atomic_b", Name: "atomic-b", Scopes: []string{"chat"}}
	for _, role := range []auth.Role{roleA, roleB} {
		if err := repo.CreateRole(ctx, tenant, role); err != nil {
			t.Fatalf("create role %s: %v", role.ID, err)
		}
	}

	// One valid + one unknown role id refuses the whole write.
	_, err := repo.CreateUserWithRoles(ctx, tenant, auth.User{
		ID: "user_atomic_fail", Email: "fail@example.com", Status: "active",
	}, []string{roleA.ID, "role_missing"})
	if !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("unknown role: %v, want ErrNotFound", err)
	}
	if _, err := repo.GetUserByEmail(ctx, tenant, "fail@example.com"); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("rolled-back user must not exist: %v", err)
	}

	// All-valid roles commit the user and every membership atomically.
	created, err := repo.CreateUserWithRoles(ctx, tenant, auth.User{
		ID: "user_atomic_ok", Email: "ok@example.com", Status: "active", Team: "atlas",
	}, []string{roleA.ID, roleB.ID})
	if err != nil {
		t.Fatalf("create with roles: %v", err)
	}
	if created.CreatedAt.IsZero() {
		t.Fatal("created user must carry stored created_at")
	}
	stored, err := repo.GetUserByEmail(ctx, tenant, "ok@example.com")
	if err != nil {
		t.Fatalf("fetch created user: %v", err)
	}
	if !created.CreatedAt.Equal(stored.CreatedAt) {
		t.Fatalf("created_at %v != stored %v", created.CreatedAt, stored.CreatedAt)
	}
	roles, err := repo.RolesForUser(ctx, tenant, created.ID)
	if err != nil {
		t.Fatalf("roles for user: %v", err)
	}
	if len(roles) != 2 {
		t.Fatalf("expected 2 memberships, got %v", roles)
	}
}
