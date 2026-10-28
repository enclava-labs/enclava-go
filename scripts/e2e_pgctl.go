// Command e2e_pgctl provisions and tears down the isolated PostgreSQL schema
// that scripts/dashboard_e2e.sh runs against. The harness has no psql
// dependency: schema lifecycle and the two metadata lookups the wrapper
// needs (bootstrap tenant id, run-owned qdrant_collection names) go through
// the module's own pgx driver.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: e2e_pgctl <create|drop|query> <dsn> [arg]")
		os.Exit(2)
	}
	cmd, dsn := os.Args[1], os.Args[2]

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		fail("open", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		fail("ping", err)
	}

	switch cmd {
	case "create":
		if len(os.Args) != 4 {
			fail("usage", fmt.Errorf("create needs a schema name"))
		}
		schema := os.Args[3]
		if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
			fail("create schema", err)
		}
		fmt.Println(scopedDSN(dsn, schema))
	case "drop":
		if len(os.Args) != 4 {
			fail("usage", fmt.Errorf("drop needs a schema name"))
		}
		if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS "`+os.Args[3]+`" CASCADE`); err != nil {
			fail("drop schema", err)
		}
	case "query":
		if len(os.Args) != 4 {
			fail("usage", fmt.Errorf("query needs a SQL string"))
		}
		rows, err := db.QueryContext(ctx, os.Args[3])
		if err != nil {
			fail("query", err)
		}
		defer rows.Close()
		for rows.Next() {
			var v sql.NullString
			if err := rows.Scan(&v); err != nil {
				fail("scan", err)
			}
			fmt.Println(v.String)
		}
		if err := rows.Err(); err != nil {
			fail("rows", err)
		}
	default:
		fail("usage", fmt.Errorf("unknown command %q", cmd))
	}
}

// scopedDSN appends search_path=<schema> to a postgres:// URL or key=value
// DSN — the same pinning internal/testdb uses for isolated test schemas.
func scopedDSN(dsn, schema string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			fail("parse dsn", err)
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " search_path=" + schema
}

func fail(op string, err error) {
	fmt.Fprintf(os.Stderr, "e2e_pgctl %s: %v\n", op, err)
	os.Exit(1)
}
