package connectors

import (
	"context"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
	"enclava-go/internal/testdb"
)

func TestBindAndRevokeObservedSourcePrincipal(t *testing.T) {
	f := newConnectorFixture(t)
	ctx := context.Background()
	other := testdb.SeedUser(t, f.db, f.tenant, "mapped@example.test", nil)
	if err := f.sourceRepo.InsertSourceIdentity(ctx, source.SourceIdentity{TenantID: f.tenant, ConnectionID: f.connection.ID, ID: "observed-user", NativeID: "provider-user-17"}); err != nil {
		t.Fatal(err)
	}
	admin := f.principal
	admin.Scopes[auth.ScopePlatformAdmin] = struct{}{}
	bound, err := f.svc.BindSourcePrincipal(ctx, admin, BindSourcePrincipalInput{ConnectionID: f.connection.ID, NativeUserID: "provider-user-17", MappedUserID: other.ID, VerificationEvidence: "verified in provider admin console"})
	if err != nil {
		t.Fatal(err)
	}
	if bound.MappedUserID != other.ID {
		t.Fatalf("mapping = %q", bound.MappedUserID)
	}
	if _, err := f.sourceRepo.SourceIdentityForUser(ctx, f.tenant, f.connection.ID, other.ID); err != nil {
		t.Fatalf("binding did not authorize identity lookup: %v", err)
	}
	if err := f.svc.RevokeSourceBinding(ctx, admin, bound.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sourceRepo.SourceIdentityForUser(ctx, f.tenant, f.connection.ID, other.ID); err == nil {
		t.Fatal("revoked source mapping remained usable")
	}
}
