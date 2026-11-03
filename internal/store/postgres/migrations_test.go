package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"enclava-go/internal/auth"
)

// TestMigration023AdminBackfill upgrades a controlled pre-023 schema
// (migrations < 023 only) that already holds an admin-scoped role and a
// member role. After 023 the admin role must carry mode:all on all four
// grant kinds (stored administrator authority — D-25), the member role an
// explicit deny, and a live principal must exercise exactly that authority.
// Removing the 023 backfill UPDATE fails this test.
func TestMigration023AdminBackfill(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("ENCLAVA_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Fatalf("ENCLAVA_TEST_DATABASE_URL is not set; migration tests require a real PostgreSQL")
	}
	ctx := context.Background()

	adminDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer func() { _ = adminDB.Close() }()
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("generate schema name: %v", err)
	}
	schema := "test_" + hex.EncodeToString(buf[:])
	if _, err := adminDB.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`)
	})

	scoped, err := sql.Open("pgx", dsnWithSearchPath(dsn, schema))
	if err != nil {
		t.Fatalf("open scoped connection: %v", err)
	}
	defer func() { _ = scoped.Close() }()
	db := &DB{SQL: scoped}

	// Apply the pre-023 world: every embedded migration except 023, via the
	// same runner production uses over an injected filesystem.
	pre023 := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() == "023_role_resource_grants.sql" {
			continue
		}
		content, err := fs.ReadFile(migrationsFS, "migrations/"+entry.Name())
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		pre023["migrations/"+entry.Name()] = &fstest.MapFile{Data: content}
	}
	if err := db.applyMigrations(ctx, pre023); err != nil {
		t.Fatalf("apply pre-023 migrations: %v", err)
	}
	if _, err := scoped.ExecContext(ctx, `SELECT allowed_models FROM roles LIMIT 0`); err == nil {
		t.Fatalf("pre-023 schema must not carry grant columns")
	}

	// Seed the pre-023 state: one admin-scoped role, one member role, one
	// user per role.
	seed := []string{
		`INSERT INTO tenants (id, slug, display_name) VALUES ('11111111-1111-4111-8111-111111111111', 'mig023', 'Migration 023')`,
		`INSERT INTO roles (tenant_id, id, name, scopes) VALUES
		 ('11111111-1111-4111-8111-111111111111', 'role_admin_pre', 'Administrator', '["platform.admin"]'::jsonb)`,
		`INSERT INTO roles (tenant_id, id, name, scopes) VALUES
		 ('11111111-1111-4111-8111-111111111111', 'role_member_pre', 'Member', '["models.list"]'::jsonb)`,
		`INSERT INTO users (tenant_id, id, email, status) VALUES
		 ('11111111-1111-4111-8111-111111111111', 'user_admin_pre', 'admin@mig023.test', 'active')`,
		`INSERT INTO users (tenant_id, id, email, status) VALUES
		 ('11111111-1111-4111-8111-111111111111', 'user_member_pre', 'member@mig023.test', 'active')`,
		`INSERT INTO user_roles (tenant_id, user_id, role_id) VALUES
		 ('11111111-1111-4111-8111-111111111111', 'user_admin_pre', 'role_admin_pre')`,
		`INSERT INTO user_roles (tenant_id, user_id, role_id) VALUES
		 ('11111111-1111-4111-8111-111111111111', 'user_member_pre', 'role_member_pre')`,
	}
	for _, stmt := range seed {
		if _, err := scoped.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed pre-023 state: %v (%s)", err, stmt)
		}
	}

	// Apply 023 (plus anything after — there is none).
	if err := db.ApplyMigrations(ctx); err != nil {
		t.Fatalf("apply 023: %v", err)
	}

	repo := NewIdentityRepo(db)
	const tenant = "11111111-1111-4111-8111-111111111111"

	adminRole, err := repo.GetRole(ctx, tenant, "role_admin_pre")
	if err != nil {
		t.Fatalf("get admin role: %v", err)
	}
	for name, a := range map[string]auth.Allowlist{
		"models": adminRole.Grants.Models, "endpoints": adminRole.Grants.Endpoints,
		"assistants": adminRole.Grants.Assistants, "templates": adminRole.Grants.ExtractTemplates,
	} {
		if !a.All {
			t.Fatalf("pre-023 admin role lost mode:all on %s: %+v", name, adminRole.Grants)
		}
	}

	memberRole, err := repo.GetRole(ctx, tenant, "role_member_pre")
	if err != nil {
		t.Fatalf("get member role: %v", err)
	}
	for name, a := range map[string]auth.Allowlist{
		"models": memberRole.Grants.Models, "endpoints": memberRole.Grants.Endpoints,
		"assistants": memberRole.Grants.Assistants, "templates": memberRole.Grants.ExtractTemplates,
	} {
		if a.All || len(a.Items) != 0 {
			t.Fatalf("pre-023 member role must deny %s, got %+v", name, a)
		}
	}

	// The preserved authority is live: an admin principal is unrestricted
	// via stored grants alone; a member principal denies.
	authSvc := auth.NewService(repo, "")
	adminPrincipal, err := authSvc.PrincipalForActor(ctx, tenant, "user_admin_pre")
	if err != nil {
		t.Fatalf("admin principal: %v", err)
	}
	if !adminPrincipal.AllowedModelsAll || !adminPrincipal.AllowedEndpointsAll ||
		!adminPrincipal.AllowedChatbotsAll || !adminPrincipal.AllowedAgentsAll ||
		!adminPrincipal.AllowedExtractTemplatesAll {
		t.Fatalf("admin principal missing migrated authority: %+v", adminPrincipal)
	}

	memberPrincipal, err := authSvc.PrincipalForActor(ctx, tenant, "user_member_pre")
	if err != nil {
		t.Fatalf("member principal: %v", err)
	}
	if memberPrincipal.AllowedModelsAll || len(memberPrincipal.AllowedModels) != 0 ||
		memberPrincipal.AllowedChatbotsAll || memberPrincipal.AllowedAgentsAll ||
		memberPrincipal.AllowedExtractTemplatesAll || memberPrincipal.AllowedEndpointsAll {
		t.Fatalf("member principal must deny every resource: %+v", memberPrincipal)
	}
}
