// identity_dashboard_test.go — the shell's tenant display-name read.
package postgres_test

import (
	"context"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestGetTenantDisplayName(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	repo := postgres.NewIdentityRepo(db)

	id, err := repo.CreateTenant(ctx, "display-slug", "Acme Trading Co")
	if err != nil {
		t.Fatal(err)
	}
	name, err := repo.GetTenantDisplayName(ctx, id)
	if err != nil {
		t.Fatalf("get display name: %v", err)
	}
	if name != "Acme Trading Co" {
		t.Fatalf("display name = %q, want the stored row value", name)
	}

	// An unknown (or malformed) id is auth.ErrNotFound so the shell falls
	// back to the tenant slug.
	if _, err := repo.GetTenantDisplayName(ctx, "00000000-0000-0000-0000-000000000000"); err == nil || err != auth.ErrNotFound {
		t.Fatalf("unknown id err = %v, want auth.ErrNotFound", err)
	}
	if _, err := repo.GetTenantDisplayName(ctx, "not-a-uuid"); err == nil || err != auth.ErrNotFound {
		t.Fatalf("malformed id err = %v, want auth.ErrNotFound", err)
	}
}
