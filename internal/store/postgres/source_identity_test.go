package postgres

import (
	"context"
	"strings"
	"testing"
)

// These tests exercise the real schema: plain SQL only, no repositories
// (repos land in plan 01-09). RequireTestDB supplies a freshly migrated,
// schema-isolated database or fails the test hard.

const (
	idTenantA = "11111111-1111-1111-1111-111111111111"
	idTenantB = "22222222-2222-2222-2222-222222222222"
)

// TestMigrationsCompositeFKs proves that every foreign key on a
// tenant-owned table is a composite (tenant_id, ref_id) link — the
// database-level enforcement that a row can never point at another
// tenant's record.
func TestMigrationsCompositeFKs(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()

	var applied int
	if err := db.SQL.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM schema_migrations WHERE version IN ('002_identity.sql','003_source.sql')",
	).Scan(&applied); err != nil {
		t.Fatalf("check applied migrations: %v", err)
	}
	if applied != 2 {
		t.Fatalf("expected 002_identity.sql and 003_source.sql applied, got %d", applied)
	}

	rows, err := db.SQL.QueryContext(ctx, `
		SELECT con.conrelid::regclass::text AS child_table,
		       con.conname,
		       string_agg(att.attname, ',' ORDER BY k.ord) AS cols
		FROM pg_constraint con
		JOIN unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord) ON TRUE
		JOIN pg_attribute att ON att.attrelid = con.conrelid
		             AND att.attnum = k.attnum
		WHERE con.contype = 'f'
		  AND con.connamespace = current_schema()::regnamespace
		  AND con.confrelid <> 'tenants'::regclass
		GROUP BY con.conrelid, con.conname`)
	if err != nil {
		t.Fatalf("query foreign keys: %v", err)
	}
	defer func() { _ = rows.Close() }()

	seen := 0
	for rows.Next() {
		var table, name, colCSV string
		if err := rows.Scan(&table, &name, &colCSV); err != nil {
			t.Fatalf("scan fk: %v", err)
		}
		cols := strings.Split(colCSV, ",")
		seen++
		if len(cols) < 2 || cols[0] != "tenant_id" {
			t.Fatalf("%s.%s is not a composite (tenant_id, ...) foreign key: cols=%v",
				table, name, cols)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate foreign keys: %v", err)
	}
	if seen == 0 {
		t.Fatalf("no foreign keys found in migrated schema")
	}
}

func seedTenant(t *testing.T, ctx context.Context, db *DB, id string) {
	t.Helper()
	if _, err := db.SQL.ExecContext(ctx,
		"INSERT INTO tenants (id, slug, display_name) VALUES ($1, $2, $3)",
		id, "tenant-"+id[:8], "Tenant "+id[:8]); err != nil {
		t.Fatalf("seed tenant %s: %v", id, err)
	}
}

func seedConnection(t *testing.T, ctx context.Context, db *DB, tenant, id, kind string) {
	t.Helper()
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO connections (tenant_id, id, kind, name, credential_ref)
		VALUES ($1, $2, $3, $4, $5)`,
		tenant, id, kind, "conn "+id, "ref-"+id); err != nil {
		t.Fatalf("seed connection %s: %v", id, err)
	}
}

// TestSourceIdentityIsolation proves connection-qualified source identity:
// the same (kind, native_id) under two connections is two objects, a
// NULL-connection upload coexists with both, and uploads still dedupe
// among themselves.
func TestSourceIdentityIsolation(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()

	seedTenant(t, ctx, db, idTenantA)
	seedConnection(t, ctx, db, idTenantA, "conn_a", "github")
	seedConnection(t, ctx, db, idTenantA, "conn_b", "github")

	insert := func(connID any, id string) error {
		_, err := db.SQL.ExecContext(ctx, `
			INSERT INTO source_objects
			    (tenant_id, id, connection_id, kind, native_id,
			     authorization_mode, observed_seq)
			VALUES ($1, $2, $3, 'repo', 'acme/widgets', 'collection', 1)`,
			idTenantA, id, connID)
		return err
	}

	// Same native_id under two different connections -> two distinct objects.
	if err := insert("conn_a", "obj_a"); err != nil {
		t.Fatalf("insert conn_a object: %v", err)
	}
	if err := insert("conn_b", "obj_b"); err != nil {
		t.Fatalf("same native_id under conn_b must be a distinct object: %v", err)
	}
	// NULL-connection upload coexists with both.
	if err := insert(nil, "obj_upload"); err != nil {
		t.Fatalf("NULL-connection upload must coexist: %v", err)
	}
	var n int
	if err := db.SQL.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM source_objects WHERE tenant_id = $1 AND kind = 'repo' AND native_id = 'acme/widgets'",
		idTenantA).Scan(&n); err != nil {
		t.Fatalf("count objects: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 distinct objects, got %d", n)
	}

	// But uploads still dedupe: a second NULL-connection upload with the
	// same (kind, native_id) conflicts on the partial unique index.
	if err := insert(nil, "obj_upload_2"); err == nil {
		t.Fatalf("duplicate NULL-connection upload was accepted")
	}
}

// TestCrossTenantVersionRejected proves the composite (tenant_id, object_id)
// foreign key blocks a version row that mixes tenant A's object with
// tenant B's tenant_id.
func TestCrossTenantVersionRejected(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()

	seedTenant(t, ctx, db, idTenantA)
	seedTenant(t, ctx, db, idTenantB)
	seedConnection(t, ctx, db, idTenantA, "conn_a", "github")

	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO source_objects
		    (tenant_id, id, connection_id, kind, native_id, authorization_mode, observed_seq)
		VALUES ($1, 'obj_a', 'conn_a', 'repo', 'acme/widgets', 'collection', 1)`,
		idTenantA); err != nil {
		t.Fatalf("insert object: %v", err)
	}

	// Version under tenant B pointing at tenant A's object must fail.
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO source_object_versions
		    (tenant_id, id, object_id, observed_seq, artifact_ref, content_sha256, byte_size)
		VALUES ($1, 'ver_x', 'obj_a', 1, 'art_x', 'aa', 10)`,
		idTenantB); err == nil {
		t.Fatalf("cross-tenant version insert was not rejected by composite FK")
	}
}

// TestObservedVersionsInsertOnly proves monotonic insert-only observation:
// two observed_seq values for one object both persist (A->B->A cannot
// collapse identical content into an old job) and a duplicate
// (tenant_id, object_id, observed_seq) is rejected.
func TestObservedVersionsInsertOnly(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()

	seedTenant(t, ctx, db, idTenantA)

	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO source_objects
		    (tenant_id, id, connection_id, kind, native_id, authorization_mode, observed_seq)
		VALUES ($1, 'obj_up', NULL, 'doc', 'report.pdf', 'owner', 3)`,
		idTenantA); err != nil {
		t.Fatalf("insert object: %v", err)
	}

	insertVersion := func(id string, seq int, sha string) error {
		_, err := db.SQL.ExecContext(ctx, `
			INSERT INTO source_object_versions
			    (tenant_id, id, object_id, observed_seq, artifact_ref, content_sha256, byte_size)
			VALUES ($1, $2, 'obj_up', $3, $4, $5, 10)`,
			idTenantA, id, seq, "art_"+id, sha)
		return err
	}

	// A -> B -> A: seq 3 re-observes seq 1's bytes; both must persist.
	if err := insertVersion("v1", 1, "sha_a"); err != nil {
		t.Fatalf("insert seq 1: %v", err)
	}
	if err := insertVersion("v2", 2, "sha_b"); err != nil {
		t.Fatalf("insert seq 2: %v", err)
	}
	if err := insertVersion("v3", 3, "sha_a"); err != nil {
		t.Fatalf("identical content at seq 3 must persist as a new row: %v", err)
	}
	var n int
	if err := db.SQL.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM source_object_versions WHERE tenant_id = $1 AND object_id = 'obj_up'",
		idTenantA).Scan(&n); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 version rows, got %d", n)
	}

	// Reusing (tenant_id, object_id, observed_seq) conflicts.
	if err := insertVersion("v4", 1, "sha_c"); err == nil {
		t.Fatalf("duplicate (object, observed_seq) insert was accepted")
	}
}
