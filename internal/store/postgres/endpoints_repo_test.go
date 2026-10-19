// endpoints_repo_test.go proves tenant-scoped endpoint records on real
// PostgreSQL: CRUD, cross-tenant isolation, capability routing input and the
// independence of authorized_private from assurance_verified_at (INF-01).
package postgres

import (
	"context"
	"testing"
	"time"

	"enclava-go/internal/inference"
)

func TestEndpointsRepoCRUDAndTenantIsolation(t *testing.T) {
	db := RequireTestDB(t)
	repo := NewEndpointsRepo(db)
	ctx := context.Background()
	tenantA := seedBillingTenant(t, ctx, db)
	tenantB := seedBillingTenant(t, ctx, db)

	verified := time.Now().UTC().Add(-time.Hour)
	e := inference.Endpoint{
		ID: "ep-1", TenantID: tenantA, Protocol: "openai",
		BaseOrigin: "https://api.local.example", CredentialRef: "cred-a",
		Capabilities:        []string{"chat", "embeddings"},
		AssuranceMode:       "attested",
		AssuranceVerifiedAt: &verified,
		AuthorizedPrivate:   true, Enabled: true,
	}
	if err := repo.Create(ctx, e); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.Get(ctx, tenantA, "ep-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TenantID != tenantA || got.BaseOrigin != e.BaseOrigin ||
		got.CredentialRef != "cred-a" || got.AssuranceMode != "attested" ||
		!got.AuthorizedPrivate || !got.Enabled {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	// assurance_verified_at persists independently of authorized_private.
	if got.AssuranceVerifiedAt == nil || !got.AssuranceVerifiedAt.Equal(verified.Truncate(time.Microsecond)) {
		t.Fatalf("assurance_verified_at not persisted: %+v", got.AssuranceVerifiedAt)
	}
	if len(got.Capabilities) != 2 || got.Capabilities[0] != "chat" {
		t.Fatalf("capabilities round-trip: %v", got.Capabilities)
	}

	// Cross-tenant: tenant B sees nothing.
	if _, err := repo.Get(ctx, tenantB, "ep-1"); err == nil {
		t.Fatal("cross-tenant get must fail")
	}
	if list, err := repo.List(ctx, tenantB); err != nil || len(list) != 0 {
		t.Fatalf("cross-tenant list leaked %d endpoints (err=%v)", len(list), err)
	}

	// authorized_private persists distinctly: a public https endpoint without
	// it stores false even with the same assurance timestamp.
	e2 := e
	e2.ID = "ep-2"
	e2.AuthorizedPrivate = false
	e2.BaseOrigin = "https://api.example.com"
	if err := repo.Create(ctx, e2); err != nil {
		t.Fatalf("create ep-2: %v", err)
	}
	got2, err := repo.Get(ctx, tenantA, "ep-2")
	if err != nil {
		t.Fatalf("get ep-2: %v", err)
	}
	if got2.AuthorizedPrivate || got2.AssuranceVerifiedAt == nil {
		t.Fatalf("authorized_private/assurance conflated: %+v", got2)
	}

	// Update persists changed assurance independently.
	if err := repo.Update(ctx, inference.Endpoint{
		ID: "ep-2", TenantID: tenantA, Protocol: "openai",
		BaseOrigin: "https://api.example.com", CredentialRef: "cred-b",
		Capabilities: []string{"chat"}, AssuranceMode: "none",
		Enabled: true,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, err = repo.Get(ctx, tenantA, "ep-2")
	if err != nil {
		t.Fatalf("get updated: %v", err)
	}
	if got2.AssuranceMode != "none" || got2.AssuranceVerifiedAt != nil || got2.CredentialRef != "cred-b" {
		t.Fatalf("update round-trip: %+v", got2)
	}

	// SetEnabled + ListCapabilities: disabled endpoints leave routing input.
	if err := repo.SetEnabled(ctx, tenantA, "ep-1", false); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	caps, err := repo.ListCapabilities(ctx, tenantA, "embeddings")
	if err != nil {
		t.Fatalf("list capabilities: %v", err)
	}
	if len(caps) != 0 {
		t.Fatalf("disabled endpoint still advertised: %+v", caps)
	}
	caps, err = repo.ListCapabilities(ctx, tenantA, "chat")
	if err != nil || len(caps) != 1 || caps[0].ID != "ep-2" {
		t.Fatalf("capability routing: %+v err=%v", caps, err)
	}
}

func TestEndpointsRepoOriginValidation(t *testing.T) {
	db := RequireTestDB(t)
	repo := NewEndpointsRepo(db)
	ctx := context.Background()
	tenant := seedBillingTenant(t, ctx, db)

	base := inference.Endpoint{
		ID: "bad", TenantID: tenant, Protocol: "openai",
		Capabilities: []string{"chat"}, Enabled: true,
	}
	for _, tc := range []struct {
		name    string
		origin  string
		priv    bool
		wantErr bool
	}{
		{"https absolute origin", "https://api.example.com", false, false},
		{"http public rejected", "http://api.example.com", false, true},
		{"http authorized private allowed", "http://10.0.0.9:8080", true, false},
		{"relative url rejected", "example.com/api", false, true},
		{"path rejected", "https://api.example.com/v1", false, true},
		{"query rejected", "https://api.example.com?x=1", false, true},
		{"userinfo rejected", "https://u:p@api.example.com", false, true},
		{"ftp rejected", "ftp://api.example.com", false, true},
		{"empty rejected", "", false, true},
	} {
		e := base
		e.ID = tc.name
		e.BaseOrigin = tc.origin
		e.AuthorizedPrivate = tc.priv
		err := repo.Create(ctx, e)
		if tc.wantErr && err == nil {
			t.Errorf("%s: expected rejection", tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s: unexpected rejection %v", tc.name, err)
		}
	}
}
