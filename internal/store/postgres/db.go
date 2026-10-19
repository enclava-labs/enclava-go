// Package postgres provides the primary application store: a bounded
// *sql.DB over pgx with embedded transactional migrations.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type DB struct {
	SQL *sql.DB
}

// Open validates a PostgreSQL DSN (postgres://, postgresql:// or key=value),
// applies embedded migrations and returns a bounded pool. It fails closed on
// empty strings, filesystem paths and file: URIs — there is no SQLite path.
func Open(ctx context.Context, dsn string) (*DB, error) {
	dsn = strings.TrimSpace(dsn)
	if err := validateDSN(dsn); err != nil {
		return nil, err
	}

	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	sqldb.SetMaxOpenConns(20)
	sqldb.SetMaxIdleConns(5)
	sqldb.SetConnMaxLifetime(30 * time.Minute)

	if err := sqldb.PingContext(ctx); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	db := &DB{SQL: sqldb}
	if err := db.ApplyMigrations(ctx); err != nil {
		_ = sqldb.Close()
		return nil, err
	}
	return db, nil
}

func validateDSN(dsn string) error {
	if dsn == "" {
		return fmt.Errorf("postgres DSN is empty")
	}
	if strings.HasPrefix(dsn, "file:") || strings.HasPrefix(dsn, "/") ||
		strings.HasPrefix(dsn, "./") || strings.HasPrefix(dsn, "~") ||
		strings.HasSuffix(strings.ToLower(dsn), ".db") {
		return fmt.Errorf("postgres DSN must not be a filesystem path or file: URI: %q", dsn)
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		if _, err := url.Parse(dsn); err != nil {
			return fmt.Errorf("invalid postgres URL: %w", err)
		}
		return nil
	}
	if !strings.Contains(dsn, "=") {
		return fmt.Errorf("postgres DSN must be a postgres:// URL or key=value string: %q", dsn)
	}
	return nil
}

func (db *DB) Close() error {
	return db.SQL.Close()
}
