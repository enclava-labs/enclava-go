package postgres_test

import (
	"context"
	"encoding/json"
	"testing"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/connectors"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestConnectorStreamPagesPersistBeforeCursorAndFenceOldRuns(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	tenant := testdb.SeedTenant(t, db, "connector-stream")
	connections := postgres.NewSourceRepo(db)
	const connectionID = "00000000-0000-4000-8000-000000000010"
	if err := connections.CreateConnection(ctx, source.Connection{TenantID: tenant, ID: connectionID, Kind: source.ConnectionSlack, Name: "Slack", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	repo := postgres.NewConnectorObservationsRepo(db)
	first, err := repo.StartRun(ctx, tenant, connectionID, string(connectors.StreamContent), "00000000-0000-4000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	first, err = repo.PersistPage(ctx, first, []postgres.ConnectorObservation{{NativeID: "message-1", Payload: json.RawMessage(`{"text":"hello"}`), ArtifactRef: artifacts.Ref{TenantID: tenant, ID: "artifact-1", Kind: artifacts.KindOriginal, Version: 1}}}, "opaque-next", false, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Cursor != "opaque-next" || first.ObservedSeq != 1 || first.Complete || !first.Incomplete {
		t.Fatalf("unexpected incomplete checkpoint: %#v", first)
	}
	second, err := repo.StartRun(ctx, tenant, connectionID, string(connectors.StreamContent), "00000000-0000-4000-8000-000000000002")
	if err != nil {
		t.Fatal(err)
	}
	if second.Cursor != "opaque-next" || second.ObservedSeq != 1 || second.Fence <= first.Fence {
		t.Fatalf("resume did not preserve checkpoint and advance fence: %#v", second)
	}
	if _, err := repo.PersistPage(ctx, first, nil, "stale", true, false, ""); err == nil {
		t.Fatal("stale run advanced stream checkpoint")
	}
	second, err = repo.PersistPage(ctx, second, nil, "", true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := repo.GetStream(ctx, tenant, connectionID, string(connectors.StreamContent))
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Complete || stored.Incomplete || stored.ObservedSeq != 1 || stored.RunID != second.RunID {
		t.Fatalf("terminal checkpoint mismatch: %#v", stored)
	}
}
