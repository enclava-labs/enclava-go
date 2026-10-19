package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
)

// RequireTestDB returns a store migrated inside a fresh, randomly named
// schema on the shared ENCLAVA_TEST_DATABASE_URL database. It fails the test
// hard when the variable is unset or unreachable — nothing silently skips.
func RequireTestDB(t *testing.T) *DB {
	t.Helper()
	db, _ := openIsolated(t)
	return db
}

// openIsolated creates a random schema, migrates it and returns the store
// plus the schema-scoped DSN for second-connection checks.
func openIsolated(t *testing.T) (*DB, string) {
	t.Helper()
	ctx := context.Background()

	dsn := strings.TrimSpace(os.Getenv("ENCLAVA_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Fatalf("ENCLAVA_TEST_DATABASE_URL is not set; repository tests require a real PostgreSQL")
	}

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}

	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("generate schema name: %v", err)
	}
	schema := "test_" + hex.EncodeToString(buf[:])

	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatalf("create test schema %s: %v", schema, err)
	}

	scoped := dsnWithSearchPath(dsn, schema)
	db, err := Open(ctx, scoped)
	if err != nil {
		if _, derr := admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`); derr != nil {
			t.Fatalf("open test store: %v (cleanup also failed: %v)", err, derr)
		}
		t.Fatalf("open test store in schema %s: %v", schema, err)
	}

	t.Cleanup(func() {
		_ = db.Close()
		if _, err := admin.ExecContext(context.Background(),
			`DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`); err != nil {
			t.Logf("drop test schema %s: %v", schema, err)
		}
		_ = admin.Close()
	})
	return db, scoped
}

func dsnWithSearchPath(dsn, schema string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return dsn
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " search_path=" + schema
}
