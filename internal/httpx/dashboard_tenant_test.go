// dashboard_tenant_test.go — the shell's userrow and org-name assembly.
package httpx

import (
	"context"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestDashboardUser(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	repo := postgres.NewIdentityRepo(db)
	tenant, err := repo.CreateTenant(ctx, "shell-org", "Northwind Trading")
	if err != nil {
		t.Fatal(err)
	}
	user := auth.User{ID: "user_shell1", TenantID: tenant, Email: "maja@northwind.example", DisplayName: "Maja Novak", Status: "active"}
	if err := repo.CreateUser(ctx, tenant, user); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateRole(ctx, tenant, auth.Role{ID: "role_admin1", Name: "Admin", Scopes: []string{"platform.admin"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.AssignRole(ctx, tenant, user.ID, "role_admin1"); err != nil {
		t.Fatal(err)
	}

	app := &App{identity: repo}
	u := app.dashboardUserFor(ctx, auth.Principal{TenantID: tenant, UserID: user.ID})
	if u.Name != "Maja Novak" || u.Email != "maja@northwind.example" {
		t.Fatalf("user = %+v, want repo display name + email", u)
	}
	if u.RoleLabel != "Admin" {
		t.Fatalf("role label = %q, want joined role names", u.RoleLabel)
	}
	if u.Initials != "MN" {
		t.Fatalf("initials = %q, want MN", u.Initials)
	}

	// Unknown user: falls back to the principal identity, never errors.
	fallback := app.dashboardUserFor(ctx, auth.Principal{TenantID: tenant, UserID: "user_ghost"})
	if fallback.Name != "user_ghost" || fallback.RoleLabel != "—" {
		t.Fatalf("fallback user = %+v, want principal id + em-dash role", fallback)
	}
	if got := dashboardInitials("", "maja@northwind.example"); got != "MA" {
		t.Fatalf("email initials = %q, want MA", got)
	}

	if org := app.dashboardOrgName(ctx, tenant); org != "Northwind Trading" {
		t.Fatalf("org = %q, want the tenants row display name", org)
	}
	// ErrNotFound → slug fallback humanized; absent slug → product name.
	if org := app.dashboardOrgName(ctx, "00000000-0000-0000-0000-000000000000"); org != "Enclava" {
		t.Fatalf("org fallback = %q, want Enclava", org)
	}
}
