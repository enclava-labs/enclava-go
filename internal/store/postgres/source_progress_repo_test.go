package postgres_test

import (
	"context"
	"testing"

	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

// TestSourceIndexProgressEmptySource: a source that has synced nothing yet
// reports zero counts, not "no rows" (the Sources page must still render it).
func TestSourceIndexProgressEmptySource(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	tenant := testdb.SeedTenant(t, db, "progress-empty")
	repo := postgres.NewSourceRepo(db)
	const connectionID = "00000000-0000-4000-8000-000000000020"
	if err := repo.CreateConnection(ctx, source.Connection{TenantID: tenant, ID: connectionID, Kind: source.ConnectionSlack, Name: "Slack", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	got, err := repo.SourceIndexProgress(ctx, tenant, connectionID, "ragcol_none")
	if err != nil || got != (source.IndexProgress{}) {
		t.Fatalf("progress = %+v, %v; want zero counts", got, err)
	}
}
