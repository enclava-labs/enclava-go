package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
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
	first, err := repo.StartRun(ctx, tenant, connectionID, string(connectors.StreamContent), "00000000-0000-4000-8000-000000000001", "config-a")
	if err != nil {
		t.Fatal(err)
	}
	first, err = repo.PersistPage(ctx, first, []postgres.ConnectorObservation{{TenantID: tenant, ConnectionID: connectionID, Stream: string(connectors.StreamContent), RunID: first.RunID, NativeID: "message-1", ObjectRef: source.ObjectRef{TenantID: tenant, ObjectID: "object-message-1", ObservedSeq: 1}, Payload: json.RawMessage(`{"text":"hello"}`), ArtifactRef: artifacts.Ref{TenantID: tenant, ID: "artifact-1", Kind: artifacts.KindOriginal, Version: 1}}}, "opaque-next", false, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Cursor != "opaque-next" || first.ObservedSeq != 1 || first.Complete || !first.Incomplete {
		t.Fatalf("unexpected incomplete checkpoint: %#v", first)
	}
	second, err := repo.StartRun(ctx, tenant, connectionID, string(connectors.StreamContent), "00000000-0000-4000-8000-000000000002", "config-a")
	if err != nil {
		t.Fatal(err)
	}
	if second.Cursor != "opaque-next" || second.ObservedSeq != 1 || second.Fence <= first.Fence {
		t.Fatalf("resume did not preserve checkpoint and advance fence: %#v", second)
	}
	if _, err := repo.PersistPage(ctx, first, nil, "stale", true, false, ""); err == nil {
		t.Fatal("stale run advanced stream checkpoint")
	}
	second, err = repo.PersistPage(ctx, second, nil, "delta-checkpoint", true, false, "")
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
	third, err := repo.StartRun(ctx, tenant, connectionID, string(connectors.StreamContent), "00000000-0000-4000-8000-000000000003", "config-a")
	if err != nil {
		t.Fatal(err)
	}
	if third.Cursor != "delta-checkpoint" || third.RunStartSeq != 1 || third.Fence <= second.Fence {
		t.Fatalf("new run did not retain delta checkpoint and reset observation boundary: %#v", third)
	}
	currentRun, err := repo.ListObservations(ctx, third)
	if err != nil {
		t.Fatal(err)
	}
	if len(currentRun) != 0 {
		t.Fatalf("new scan inherited observations from prior A→B→A history: %#v", currentRun)
	}
	membership, err := repo.StartRun(ctx, tenant, connectionID, string(connectors.StreamMembership), "00000000-0000-4000-8000-000000000004", "config-a")
	if err != nil {
		t.Fatal(err)
	}
	if membership.Fence == 0 || membership.Cursor != "" {
		t.Fatalf("membership stream did not initialize independently: %#v", membership)
	}
	stillContent, err := repo.GetStream(ctx, tenant, connectionID, string(connectors.StreamContent))
	if err != nil {
		t.Fatal(err)
	}
	if stillContent.RunID != third.RunID || stillContent.Fence != third.Fence {
		t.Fatalf("membership run interfered with content stream: %#v", stillContent)
	}

	third, err = repo.PersistPage(ctx, third, []postgres.ConnectorObservation{{NativeID: "old-config", Payload: json.RawMessage(`{}`)}}, "old-cursor", false, true, "")
	if err != nil {
		t.Fatal(err)
	}
	changed, err := repo.StartRun(ctx, tenant, connectionID, string(connectors.StreamContent), "00000000-0000-4000-8000-000000000005", "config-b")
	if err != nil {
		t.Fatal(err)
	}
	if changed.Cursor != "" || changed.RunStartSeq != third.ObservedSeq || changed.ObservedSeq != third.ObservedSeq || changed.Fence <= third.Fence {
		t.Fatalf("configuration change did not reset scan boundary: %#v", changed)
	}
	if observations, err := repo.ListObservations(ctx, changed); err != nil || len(observations) != 0 {
		t.Fatalf("configuration change inherited observations: %#v, %v", observations, err)
	}
	if _, err := repo.PersistPage(ctx, third, nil, "stale", true, false, ""); err == nil {
		t.Fatal("old configuration run retained its fence")
	}
}

func TestConnectorUnmappedPrincipalsCountsDistinctUnboundAccounts(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	tenant := testdb.SeedTenant(t, db, "connector-unmapped")
	other := testdb.SeedTenant(t, db, "connector-unmapped-other")
	user := testdb.SeedUser(t, db, tenant, "bound@example.test", nil)
	sources := postgres.NewSourceRepo(db)
	for _, id := range []string{tenant, other} {
		if err := sources.CreateConnection(ctx, source.Connection{TenantID: id, ID: "conn", Kind: source.ConnectionSlack, Name: "Slack", Status: "active"}); err != nil {
			t.Fatal(err)
		}
	}
	repo := postgres.NewConnectorObservationsRepo(db)
	if count, err := repo.UnmappedPrincipals(ctx, tenant, "conn"); err != nil || count != 0 {
		t.Fatalf("empty count = %d, %v", count, err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.SQL.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO source_objects (tenant_id, id, connection_id, kind, native_id, authorization_mode, observed_seq)
		VALUES ($1, 'obj', 'conn', 'channel', 'slack:C1', 'source_acl', 1), ($2, 'obj', 'conn', 'channel', 'slack:C1', 'source_acl', 1)`, tenant, other)
	exec(`INSERT INTO source_groups (tenant_id, id, connection_id, source_group_native_id, name) VALUES ($1, 'grp', 'conn', 'C1', 'C1')`, tenant)
	exec(`INSERT INTO source_identities (tenant_id, id, connection_id, source_user_native_id, mapped_user_id)
		VALUES ($1, 'both', 'conn', 'U1', NULL), ($1, 'member', 'conn', 'U2', NULL), ($1, 'bound', 'conn', 'U3', $2),
		       ($1, 'stale', 'conn', 'U4', NULL), ($1, 'pending', 'conn', 'U5', NULL)`, tenant, user.ID)
	exec(`INSERT INTO source_identities (tenant_id, id, connection_id, source_user_native_id) VALUES ($1, 'elsewhere', 'conn', 'U9')`, other)
	exec(`INSERT INTO access_snapshots (tenant_id, id, connection_id, kind, revision, status)
		VALUES ($1, 'g1', 'conn', 'grants', 1, 'complete'), ($1, 'g2', 'conn', 'grants', 2, 'complete'),
		       ($1, 'g3', 'conn', 'grants', 3, 'incomplete'), ($1, 'm1', 'conn', 'membership', 1, 'complete'),
		       ($2, 'g9', 'conn', 'grants', 9, 'complete')`, tenant, other)
	grant := func(tenantID, id, identity string, revision int) {
		exec(`INSERT INTO source_grants (tenant_id, id, object_id, subject_type, source_identity_id, effect, access_revision)
			VALUES ($1, $2, 'obj', 'user', $3, 'allow', $4)`, tenantID, id, identity, revision)
	}
	grant(tenant, "gr-stale", "stale", 1)     // superseded revision
	grant(tenant, "gr-both", "both", 2)       // also a member below
	grant(tenant, "gr-bound", "bound", 2)     // has an Enclava binding
	grant(tenant, "gr-pending", "pending", 3) // incomplete revision
	grant(other, "gr-elsewhere", "elsewhere", 9)
	exec(`INSERT INTO source_group_memberships (tenant_id, id, group_id, source_identity_id, access_revision)
		VALUES ($1, 'm-both', 'grp', 'both', 1), ($1, 'm-member', 'grp', 'member', 1)`, tenant)
	if count, err := repo.UnmappedPrincipals(ctx, tenant, "conn"); err != nil || count != 2 {
		t.Fatalf("distinct unbound accounts = %d, %v; want 2 (both, member)", count, err)
	}
}

func TestConnectorIdentityResolverPropagatesDatabaseErrors(t *testing.T) {
	db := testdb.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := postgres.NewSourceRepo(db).SourceIdentityForNativeID(ctx, "00000000-0000-4000-8000-000000000001", "conn", "user")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("resolver error = %v, want context.Canceled", err)
	}
}
