package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestDeleteUserPreservesHistoryAndRevokesCredentials(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	repo := postgres.NewIdentityRepo(db)
	svc := auth.NewService(repo, "")
	tenant := testdb.SeedTenant(t, db, "delete-user")
	otherTenant := testdb.SeedTenant(t, db, "delete-user-other")
	user := testdb.SeedUser(t, db, tenant, "deleted@example.com", []string{"models.list"})
	hash, err := auth.HashPassword("test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetPassword(ctx, tenant, user.ID, hash, time.Now()); err != nil {
		t.Fatal(err)
	}
	key, record := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "key", OwnerUserID: user.ID, Scopes: []string{"models.list"}})
	token, _, sessionID, err := svc.IssueSession(ctx, tenant, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteUserAtomically(ctx, otherTenant, user.ID); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("cross-tenant deletion: %v", err)
	}
	if _, err := svc.Authenticate(ctx, key); err != nil {
		t.Fatalf("key before deletion: %v", err)
	}
	if err := repo.DeleteUserAtomically(ctx, tenant, user.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteUserAtomically(ctx, tenant, user.ID); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("repeat deletion: %v", err)
	}
	if _, err := svc.AuthenticatePassword(ctx, tenant, user.Email, "test-password"); err == nil {
		t.Fatal("deleted user's password authenticated")
	}
	if err := repo.CreateAPIKey(ctx, tenant, record); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("create deleted user's key: %v", err)
	}
	if _, err := svc.Authenticate(ctx, key); err == nil {
		t.Fatal("deleted user's key authenticated")
	}
	if _, err := svc.AuthenticateSession(ctx, token); err == nil {
		t.Fatal("deleted user's session authenticated")
	}
	if _, _, _, err := svc.IssueSession(ctx, tenant, user.ID, time.Hour); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("issue deleted user's session: %v", err)
	}
	if _, err := repo.GetUser(ctx, tenant, user.ID); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("deleted user lookup: %v", err)
	}
	if _, err := repo.GetUserByEmail(ctx, tenant, user.Email); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("deleted email lookup: %v", err)
	}
	users, err := repo.ListUsers(ctx, tenant)
	if err != nil || len(users) != 0 {
		t.Fatalf("deleted user listed: %v %v", users, err)
	}
	var deleted, sessionRevoked, keyRevoked bool
	var roles int
	err = db.SQL.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL,
		(SELECT revoked_at IS NOT NULL FROM sessions WHERE tenant_id=$1::uuid AND id=$3),
		(SELECT revoked_at IS NOT NULL FROM api_keys WHERE tenant_id=$1::uuid AND id=$4),
		(SELECT count(*) FROM user_roles WHERE tenant_id=$1::uuid AND user_id=$2)
		FROM users WHERE tenant_id=$1::uuid AND id=$2`, tenant, user.ID, sessionID, record.ID).Scan(&deleted, &sessionRevoked, &keyRevoked, &roles)
	if err != nil || !deleted || !sessionRevoked || !keyRevoked || roles != 0 {
		t.Fatalf("history/revocation: %v %v %v roles=%d err=%v", deleted, sessionRevoked, keyRevoked, roles, err)
	}
	replacement := testdb.SeedUser(t, db, tenant, user.Email, nil)
	if replacement.ID == user.ID {
		t.Fatal("replacement reused deleted identity")
	}
}

func TestDeleteUserLastAdminAndConcurrentDeletes(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	repo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "delete-admin")
	first := testdb.SeedUser(t, db, tenant, "first@example.com", []string{auth.ScopePlatformAdmin})
	testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "admin-key", OwnerUserID: first.ID, Scopes: []string{auth.ScopePlatformAdmin}})
	if err := repo.DeleteUserAtomically(ctx, tenant, first.ID); !errors.Is(err, postgres.ErrLastAdmin) {
		t.Fatalf("last admin: %v", err)
	}
	second := testdb.SeedUser(t, db, tenant, "second@example.com", []string{"*"})
	results := make(chan error, 2)
	go func() { results <- repo.DeleteUserAtomically(ctx, tenant, first.ID) }()
	go func() { results <- repo.DeleteUserAtomically(ctx, tenant, second.ID) }()
	a, b := <-results, <-results
	if (a != nil || !errors.Is(b, postgres.ErrLastAdmin)) && (b != nil || !errors.Is(a, postgres.ErrLastAdmin)) {
		t.Fatalf("concurrent deletes: %v, %v", a, b)
	}
}
