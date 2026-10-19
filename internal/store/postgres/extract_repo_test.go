package postgres_test

import (
	"context"
	"errors"
	"testing"

	"enclava-go/internal/extract"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestExtractRepoTemplatesTenantScoped(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	repo := postgres.NewExtractRepo(db)
	tenantA := testdb.SeedTenant(t, db, "tenant-a")
	tenantB := testdb.SeedTenant(t, db, "tenant-b")

	tpl := extract.Template{
		ID: "invoice", Name: "Invoice", Description: "d",
		SystemPrompt: "s", UserPrompt: "u",
		ContextSchema: map[string]any{"company": map[string]any{"type": "string"}},
		OutputSchema:  map[string]any{"type": "object", "properties": map[string]any{"total": map[string]any{"type": "number"}}},
		DefaultModel:  "m1", Enabled: true, IsDefault: false, OwnerActorID: "actor-1",
	}
	if err := repo.UpsertTemplate(ctx, tenantA, tpl); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := repo.GetTemplate(ctx, tenantA, "invoice")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Invoice" || got.DefaultModel != "m1" || !got.Enabled ||
		got.ContextSchema["company"] == nil || got.OutputSchema["type"] != "object" ||
		got.OwnerActorID != "actor-1" {
		t.Fatalf("round trip mismatch: %+v", got)
	}

	// Update path.
	tpl.Description = "d2"
	tpl.Enabled = false
	if err := repo.UpsertTemplate(ctx, tenantA, tpl); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got, _ = repo.GetTemplate(ctx, tenantA, "invoice")
	if got.Description != "d2" || got.Enabled {
		t.Fatalf("update mismatch: %+v", got)
	}

	// Tenant B sees nothing for the same id.
	if _, err := repo.GetTemplate(ctx, tenantB, "invoice"); !errors.Is(err, extract.ErrNotFound) {
		t.Fatalf("cross-tenant get: %v, want ErrNotFound", err)
	}
	listB, err := repo.ListTemplates(ctx, tenantB)
	if err != nil || len(listB) != 0 {
		t.Fatalf("cross-tenant list: %d %v", len(listB), err)
	}

	if err := repo.DeleteTemplate(ctx, tenantA, "invoice"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := repo.DeleteTemplate(ctx, tenantA, "invoice"); !errors.Is(err, extract.ErrNotFound) {
		t.Fatalf("delete missing: %v, want ErrNotFound", err)
	}
}

func TestExtractRepoReplaceDefaultsKeepsCustom(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	repo := postgres.NewExtractRepo(db)
	tenant := testdb.SeedTenant(t, db, "tenant-c")

	if err := repo.UpsertTemplate(ctx, tenant, extract.Template{
		ID: "mine", Name: "Mine", SystemPrompt: "s", UserPrompt: "u", Enabled: true,
	}); err != nil {
		t.Fatalf("custom upsert: %v", err)
	}
	if err := repo.ReplaceDefaultTemplates(ctx, tenant, extract.DefaultTemplates()); err != nil {
		t.Fatalf("replace defaults: %v", err)
	}
	list, err := repo.ListTemplates(ctx, tenant)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != len(extract.DefaultTemplates())+1 {
		t.Fatalf("list len=%d, want %d", len(list), len(extract.DefaultTemplates())+1)
	}

	// Re-running drops only the default set, then reseeds it.
	custom := extract.DefaultTemplates()
	custom[0].Name = "Renamed Default"
	if err := repo.ReplaceDefaultTemplates(ctx, tenant, custom); err != nil {
		t.Fatalf("re-replace: %v", err)
	}
	got, err := repo.GetTemplate(ctx, tenant, custom[0].ID)
	if err != nil || got.Name != "Renamed Default" {
		t.Fatalf("replaced default: %v %+v", err, got)
	}
	if _, err := repo.GetTemplate(ctx, tenant, "mine"); err != nil {
		t.Fatalf("custom template lost: %v", err)
	}
}

func TestExtractRepoSettingsUpsertRoundTrip(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	repo := postgres.NewExtractRepo(db)
	tenant := testdb.SeedTenant(t, db, "tenant-d")

	if _, err := repo.GetSettings(ctx, tenant); !errors.Is(err, extract.ErrNotFound) {
		t.Fatalf("missing settings: %v, want ErrNotFound", err)
	}
	got, err := repo.UpsertSettings(ctx, tenant, extract.Settings{DefaultModel: "m1", MaxFileSizeMB: 20})
	if err != nil {
		t.Fatalf("upsert settings: %v", err)
	}
	if got.ID != "default" || got.DefaultModel != "m1" || got.MaxFileSizeMB != 20 {
		t.Fatalf("settings mismatch: %+v", got)
	}
	got, err = repo.UpsertSettings(ctx, tenant, extract.Settings{DefaultModel: "m2", MaxFileSizeMB: 30})
	if err != nil {
		t.Fatalf("update settings: %v", err)
	}
	if got.DefaultModel != "m2" || got.MaxFileSizeMB != 30 {
		t.Fatalf("updated settings: %+v", err)
	}
	read, err := repo.GetSettings(ctx, tenant)
	if err != nil || read.DefaultModel != "m2" {
		t.Fatalf("get settings: %v %+v", err, read)
	}

	// Cross-tenant isolation for settings too.
	other := testdb.SeedTenant(t, db, "tenant-e")
	if _, err := repo.GetSettings(ctx, other); !errors.Is(err, extract.ErrNotFound) {
		t.Fatalf("cross-tenant settings: %v", err)
	}
}
