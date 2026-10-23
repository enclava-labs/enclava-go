package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"enclava-go/internal/source"
)

func TestChunkProfilesRepoImmutableTenantVersions(t *testing.T) {
	db, scopedDSN := openIsolated(t)
	ctx := context.Background()
	seedTenant(t, ctx, db, idTenantA)
	seedTenant(t, ctx, db, idTenantB)
	repo := NewChunkProfilesRepo(db)
	profile := source.ChunkProfile{
		ProfileID: "legal", SizeCodePoints: 120, OverlapCodePoints: 12,
		BoundaryStrategy: "clause", IncludeParents: true,
		TableHandling: "atomic", PreserveClauses: true,
	}
	v1, err := repo.Create(ctx, idTenantA, profile, "owner-a")
	if err != nil {
		t.Fatalf("create v1: %v", err)
	}
	profile.SizeCodePoints = 160
	profile.OverlapCodePoints = 16
	v2, err := repo.Create(ctx, idTenantA, profile, "owner-a")
	if err != nil {
		t.Fatalf("create v2: %v", err)
	}
	if v1.ID == v2.ID || v1.Version != 1 || v2.Version != 2 {
		t.Fatalf("versions must have distinct immutable row IDs and monotonic versions: v1=%+v v2=%+v", v1, v2)
	}
	if _, err := db.SQL.ExecContext(ctx, `UPDATE chunk_profiles SET max_size_codepoints=999 WHERE tenant_id=$1 AND id=$2`, idTenantA, v1.ID); err == nil {
		t.Fatal("published profile version was mutable")
	}
	gotV1, err := repo.Get(ctx, idTenantA, v1.ID)
	if err != nil || gotV1.SizeCodePoints != 120 || gotV1.OverlapCodePoints != 12 {
		t.Fatalf("v1 must retain original controls: %+v, %v", gotV1, err)
	}
	if _, err := repo.Get(ctx, idTenantB, v1.ID); !errors.Is(err, ErrChunkProfileNotFound) {
		t.Fatalf("cross-tenant read must look absent, got %v", err)
	}
	listed, err := repo.List(ctx, idTenantA)
	if err != nil || len(listed) != 2 {
		t.Fatalf("tenant list: count=%d err=%v", len(listed), err)
	}
	other, err := repo.List(ctx, idTenantB)
	if err != nil || len(other) != 0 {
		t.Fatalf("cross-tenant list exposed profiles: %#v, %v", other, err)
	}

	restarted, err := Open(ctx, scopedDSN)
	if err != nil {
		t.Fatalf("reopen isolated database: %v", err)
	}
	defer restarted.Close()
	persisted, err := NewChunkProfilesRepo(restarted).Get(ctx, idTenantA, v1.ID)
	if err != nil || persisted != gotV1 {
		t.Fatalf("profile did not survive repository restart: %+v, %v", persisted, err)
	}
}

func TestChunkProfilesRepoRollsBackFailedCreate(t *testing.T) {
	db, _ := openIsolated(t)
	db.SQL.SetMaxOpenConns(1)
	ctx := context.Background()
	seedTenant(t, ctx, db, idTenantA)
	repo := NewChunkProfilesRepo(db)
	invalid := source.ChunkProfile{ProfileID: "retry", BoundaryStrategy: "fixed", TableHandling: "row-wise"}
	if _, err := repo.Create(ctx, idTenantA, invalid, "owner-a"); err == nil {
		t.Fatal("invalid profile creation succeeded")
	}
	valid := source.ChunkProfile{ProfileID: "retry", SizeCodePoints: 120, BoundaryStrategy: "fixed", TableHandling: "row-wise"}
	retryCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	created, err := repo.Create(retryCtx, idTenantA, valid, "owner-a")
	if err != nil || created.Version != 1 {
		t.Fatalf("create after failed transaction: profile=%+v err=%v", created, err)
	}
}
