package postgres

import (
	"context"
	"database/sql"
	"testing"
	"testing/fstest"
)

func TestOpenRejectsNonDSN(t *testing.T) {
	ctx := context.Background()
	for _, dsn := range []string{"", "   ", "file:foo.db", "/tmp/foo.db", "foo.db", "relative/path.db"} {
		if _, err := Open(ctx, dsn); err == nil {
			t.Fatalf("Open(%q) succeeded; want rejection", dsn)
		}
	}
}

func TestMigrateEmptySchemaIdempotent(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()

	var n int
	if err := db.SQL.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if n == 0 {
		t.Fatalf("no migrations recorded")
	}
	if err := db.ApplyMigrations(ctx); err != nil {
		t.Fatalf("second ApplyMigrations: %v", err)
	}
	var n2 int
	if err := db.SQL.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM schema_migrations").Scan(&n2); err != nil {
		t.Fatalf("recount migrations: %v", err)
	}
	if n2 != n {
		t.Fatalf("migration count changed on re-run: %d -> %d", n, n2)
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	db, scopedDSN := openIsolated(t)
	ctx := context.Background()

	bad := fstest.MapFS{
		"migrations/999_bad.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE rolled_back (id int); BOGUS SQL;"),
		},
	}
	if err := db.applyMigrations(ctx, bad); err == nil {
		t.Fatalf("failing migration succeeded")
	}

	// Verify via a second connection on the same schema: no table, no row.
	other, err := sql.Open("pgx", scopedDSN)
	if err != nil {
		t.Fatalf("open second connection: %v", err)
	}
	defer other.Close()

	var tables int
	if err := other.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM pg_catalog.pg_tables WHERE schemaname = current_schema() AND tablename = 'rolled_back'",
	).Scan(&tables); err != nil {
		t.Fatalf("check rolled_back table: %v", err)
	}
	if tables != 0 {
		t.Fatalf("rolled_back table survived failed migration")
	}
	var rows int
	if err := other.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM schema_migrations WHERE version = '999_bad.sql'",
	).Scan(&rows); err != nil {
		t.Fatalf("check version row: %v", err)
	}
	if rows != 0 {
		t.Fatalf("version row recorded for failed migration")
	}
}

func TestTwoSchemasIsolated(t *testing.T) {
	a := RequireTestDB(t)
	b := RequireTestDB(t)
	ctx := context.Background()

	if _, err := a.SQL.ExecContext(ctx, "CREATE TABLE only_in_a (id int)"); err != nil {
		t.Fatalf("create table in schema A: %v", err)
	}
	var count int
	if err := b.SQL.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM pg_catalog.pg_tables WHERE schemaname = current_schema() AND tablename = 'only_in_a'",
	).Scan(&count); err != nil {
		t.Fatalf("check schema B: %v", err)
	}
	if count != 0 {
		t.Fatalf("table only_in_a visible in schema B")
	}
}
