// Package testdb provides the shared real-PostgreSQL fixture convention for
// all service/repository tests: New(t) creates an isolated test_<rand>
// schema on the caller-supplied ENCLAVA_TEST_DATABASE_URL, runs the embedded
// postgres migrations inside it and drops it on cleanup. It fails the test
// hard (t.Fatal, never skip) when the DSN is missing or unreachable and
// never reads the user's .env — test DSNs come from the environment only.
//
// The schema is applied with search_path pinning so every migration lands
// inside the isolated schema; each test gets a fresh, empty identity store.
package testdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"enclava-go/internal/store/postgres"
)

// New opens an isolated, migrated *postgres.DB for a test.
func New(t *testing.T) *postgres.DB {
	t.Helper()
	ctx := context.Background()

	dsn := strings.TrimSpace(os.Getenv("ENCLAVA_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Fatalf("ENCLAVA_TEST_DATABASE_URL is not set; tests require a real PostgreSQL")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("ping test database: %v", err)
	}

	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("generate schema name: %v", err)
	}
	schema := "test_" + hex.EncodeToString(buf[:])
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatalf("create test schema %s: %v", schema, err)
	}

	db, err := postgres.Open(ctx, dsnWithSearchPath(dsn, schema))
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
	return db
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
