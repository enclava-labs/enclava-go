package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// seedBindingTenant inserts the tenant row the bindings table references.
func seedBindingTenant(t *testing.T, db *DB, id string) {
	if _, err := db.SQL.ExecContext(context.Background(),
		`INSERT INTO tenants (id, slug, display_name) VALUES ($1, $2, $3)`,
		id, "tenant-"+id[24:], "Tenant "+id[24:]); err != nil {
		t.Fatalf("seed tenant %s: %v", id, err)
	}
}

func tenantForTest(n int) string {
	return fmt.Sprintf("00000000-0000-0000-0000-%012d", n)
}

func TestResponseBindingBindAndResolve(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	tenant := tenantForTest(1)
	seedBindingTenant(t, db, tenant)
	repo := NewResponseBindingsRepo(db)

	b, err := repo.Bind(ctx, ResponseBinding{
		TenantID: tenant, PublicID: "resp_pub_1", Kind: ResponseBindingResponse,
		EndpointID: "ep-a", OwnerActorID: "user_1", UpstreamID: "resp_up_1",
		InputSnapshot: []byte(`{"inputs":[{"object_id":"o1"}]}`),
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if b.State != "active" || b.CreatedAt.IsZero() {
		t.Fatalf("unexpected binding row: %+v", b)
	}
	got, err := repo.Resolve(ctx, tenant, "resp_pub_1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.UpstreamID != "resp_up_1" || got.EndpointID != "ep-a" || got.OwnerActorID != "user_1" {
		t.Fatalf("resolve mismatch: %+v", got)
	}

	// Idempotent re-bind of the same upstream reference returns the row.
	again, err := repo.Bind(ctx, ResponseBinding{
		TenantID: tenant, PublicID: "resp_pub_ignored", Kind: ResponseBindingResponse,
		EndpointID: "ep-a", OwnerActorID: "user_1", UpstreamID: "resp_up_1",
	})
	if err != nil {
		t.Fatalf("re-bind: %v", err)
	}
	if again.PublicID != "resp_pub_1" {
		t.Fatalf("re-bind must return existing row, got %q", again.PublicID)
	}
}

func TestResponseBindingUpstreamIDCollisionsStayLocal(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	tenant := tenantForTest(2)
	seedBindingTenant(t, db, tenant)
	repo := NewResponseBindingsRepo(db)

	// The same raw upstream ID on two endpoints binds to distinct public IDs.
	for i, ep := range []string{"ep-a", "ep-b"} {
		b, err := repo.Bind(ctx, ResponseBinding{
			TenantID: tenant, PublicID: fmt.Sprintf("resp_pub_%s", ep),
			Kind: ResponseBindingResponse, EndpointID: ep,
			OwnerActorID: "user_1", UpstreamID: "resp_SHARED",
		})
		if err != nil {
			t.Fatalf("bind endpoint %s: %v", ep, err)
		}
		if b.PublicID != fmt.Sprintf("resp_pub_%s", ep) || b.EndpointID != ep {
			t.Fatalf("binding %d wrong: %+v", i, b)
		}
	}
	a, err := repo.GetByUpstream(ctx, tenant, "ep-a", "", "resp_SHARED")
	if err != nil || a.PublicID != "resp_pub_ep-a" {
		t.Fatalf("ep-a upstream lookup: %v %+v", err, a)
	}
	b, err := repo.GetByUpstream(ctx, tenant, "ep-b", "", "resp_SHARED")
	if err != nil || b.PublicID != "resp_pub_ep-b" {
		t.Fatalf("ep-b upstream lookup: %v %+v", err, b)
	}
}

func TestResponseBindingRejectsForeignAndUnminted(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	tenantA, tenantB := tenantForTest(3), tenantForTest(4)
	seedBindingTenant(t, db, tenantA)
	seedBindingTenant(t, db, tenantB)
	repo := NewResponseBindingsRepo(db)

	if _, err := repo.Bind(ctx, ResponseBinding{
		TenantID: tenantA, PublicID: "resp_pub_1", Kind: ResponseBindingResponse,
		EndpointID: "ep-a", OwnerActorID: "user_1", UpstreamID: "resp_up_1",
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	// Wrong tenant: same outcome as unknown.
	if _, err := repo.Resolve(ctx, tenantB, "resp_pub_1"); !errors.Is(err, ErrResponseBindingNotFound) {
		t.Fatalf("cross-tenant resolve must not find: %v", err)
	}
	// A raw upstream ID is not a public reference.
	if _, err := repo.Resolve(ctx, tenantA, "resp_up_1"); !errors.Is(err, ErrResponseBindingNotFound) {
		t.Fatalf("raw upstream id must not resolve: %v", err)
	}
	// A public ID minted but never persisted does not exist.
	if _, err := repo.Resolve(ctx, tenantA, "resp_pub_never"); !errors.Is(err, ErrResponseBindingNotFound) {
		t.Fatalf("unbound public id must not resolve: %v", err)
	}
	// A reused public id for a different upstream reports conflict — the
	// caller remints; the first row stays intact.
	if _, err := repo.Bind(ctx, ResponseBinding{
		TenantID: tenantA, PublicID: "resp_pub_1", Kind: ResponseBindingResponse,
		EndpointID: "ep-c", OwnerActorID: "user_1", UpstreamID: "resp_other",
	}); !errors.Is(err, ErrPublicIDConflict) {
		t.Fatalf("public id reuse must conflict: %v", err)
	}
	// The conflicting insert must not have clobbered the original.
	got, err := repo.Resolve(ctx, tenantA, "resp_pub_1")
	if err != nil || got.UpstreamID != "resp_up_1" {
		t.Fatalf("conflict clobbered row: %v %+v", err, got)
	}
}

func TestResponseBindingTombstoneHides(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	tenant := tenantForTest(5)
	seedBindingTenant(t, db, tenant)
	repo := NewResponseBindingsRepo(db)

	if _, err := repo.Bind(ctx, ResponseBinding{
		TenantID: tenant, PublicID: "resp_pub_1", Kind: ResponseBindingResponse,
		EndpointID: "ep-a", OwnerActorID: "user_1", UpstreamID: "resp_up_1",
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := repo.MarkDeleted(ctx, tenant, "resp_pub_1"); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}
	if _, err := repo.Resolve(ctx, tenant, "resp_pub_1"); !errors.Is(err, ErrResponseBindingNotFound) {
		t.Fatalf("tombstoned binding must not resolve: %v", err)
	}
	if err := repo.MarkDeleted(ctx, tenant, "resp_pub_1"); !errors.Is(err, ErrResponseBindingNotFound) {
		t.Fatalf("repeat tombstone must report not-found: %v", err)
	}
	// Upstream mapping still returns the tombstoned row so replayed payloads
	// keep the same public identity instead of minting a second reference.
	got, err := repo.GetByUpstream(ctx, tenant, "ep-a", "", "resp_up_1")
	if err != nil || got.PublicID != "resp_pub_1" || got.State != "deleted" {
		t.Fatalf("upstream lookup after tombstone: %v %+v", err, got)
	}
}

func TestResponseBindingSurvivesReconnect(t *testing.T) {
	db, dsn := openIsolated(t)
	defer db.Close()
	ctx := context.Background()
	tenant := tenantForTest(6)
	seedBindingTenant(t, db, tenant)

	repo := NewResponseBindingsRepo(db)
	if _, err := repo.Bind(ctx, ResponseBinding{
		TenantID: tenant, PublicID: "resp_pub_1", Kind: ResponseBindingResponse,
		EndpointID: "ep-a", OwnerActorID: "user_1", UpstreamID: "resp_up_1",
		InputSnapshot: []byte(`{"conversations":["conv_1"],"restriction":{"required_assurance":"high"}}`),
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	item := ResponseBinding{
		TenantID: tenant, PublicID: "item_pub_1", Kind: ResponseBindingItem,
		EndpointID: "ep-a", OwnerActorID: "user_1", UpstreamID: "msg_up_1",
		ParentPublicID: "resp_pub_1",
	}
	if _, err := repo.Bind(ctx, item); err != nil {
		t.Fatalf("bind item: %v", err)
	}

	// A second connection over the same schema sees durable rows — no
	// in-process cache substitutes for persistence across restart.
	db2, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	repo2 := NewResponseBindingsRepo(db2)
	got, err := repo2.Resolve(ctx, tenant, "item_pub_1")
	if err != nil {
		t.Fatalf("resolve after reconnect: %v", err)
	}
	if got.ParentPublicID != "resp_pub_1" || got.UpstreamID != "msg_up_1" {
		t.Fatalf("reconnected row mismatch: %+v", got)
	}
	resp, err := repo2.Resolve(ctx, tenant, "resp_pub_1")
	if err != nil {
		t.Fatalf("resolve response after reconnect: %v", err)
	}
	var snap map[string]any
	if err := json.Unmarshal(resp.InputSnapshot, &snap); err != nil {
		t.Fatalf("snapshot decode: %v", err)
	}
	if snap["conversations"] == nil {
		t.Fatalf("input snapshot lost: %s", string(resp.InputSnapshot))
	}
}
