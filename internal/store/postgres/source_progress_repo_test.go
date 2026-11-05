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

// TestSourceIndexProgressInterruptedJobs: an embed job marked 'unknown' —
// interrupted after its request was sent — counts as failed with its
// message while its generation stays unpublished, but a published
// generation counts as indexed even if its job was interrupted.
func TestSourceIndexProgressInterruptedJobs(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	tenant := testdb.SeedTenant(t, db, "progress-interrupted")
	repo := postgres.NewSourceRepo(db)
	const connectionID = "00000000-0000-4000-8000-000000000021"
	if err := repo.CreateConnection(ctx, source.Connection{TenantID: tenant, ID: connectionID, Kind: source.ConnectionSlack, Name: "Slack", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	for _, seed := range []struct {
		stmt string
		args []any
	}{
		{`INSERT INTO knowledge_collections (tenant_id, id, name, qdrant_collection)
		  VALUES ($1, 'prog_col', 'Progress', 'qdr_prog_col')`, []any{tenant}},
		{`INSERT INTO source_objects (tenant_id, id, connection_id, kind, native_id, authorization_mode, observed_seq)
		  VALUES ($1, 'prog_obj1', $2, 'message', 'slack:T1', 'collection', 1),
		         ($1, 'prog_obj2', $2, 'message', 'slack:T2', 'collection', 1)`, []any{tenant, connectionID}},
		{`INSERT INTO source_object_versions (tenant_id, id, object_id, observed_seq, artifact_ref, content_sha256, byte_size)
		  VALUES ($1, 'prog_ver1', 'prog_obj1', 1, 'art1', 'sha1', 10),
		         ($1, 'prog_ver2', 'prog_obj2', 1, 'art2', 'sha2', 10)`, []any{tenant}},
		{`INSERT INTO collection_items (tenant_id, id, collection_id, object_id)
		  VALUES ($1, 'prog_ci1', 'prog_col', 'prog_obj1'), ($1, 'prog_ci2', 'prog_col', 'prog_obj2')`, []any{tenant}},
		{`INSERT INTO index_generations (tenant_id, id, collection_id, object_version_id, generation, state)
		  VALUES ($1, 'prog_gen1', 'prog_col', 'prog_ver1', 1, 'staging'),
		         ($1, 'prog_gen2', 'prog_col', 'prog_ver2', 1, 'published')`, []any{tenant}},
		{`INSERT INTO jobs (tenant_id, id, operation, status, connection_id, idempotency_key, input_hash, input_ref, principal, submitter_key, policy, error)
		  VALUES ($1, 'prog_embed_unk', 'rag.embed', 'unknown', $2, 'prog-embed-1', 'h', 'prog_ver1',
		          '{}'::jsonb, 'test-submitter', '{"generation":"1"}'::jsonb,
		          '{"message":"connection reset"}'::jsonb),
		         ($1, 'prog_embed_unk2', 'rag.embed', 'unknown', $2, 'prog-embed-2', 'h', 'prog_ver2',
		          '{}'::jsonb, 'test-submitter', '{"generation":"1"}'::jsonb, NULL)`, []any{tenant, connectionID}},
	} {
		if _, err := db.SQL.ExecContext(ctx, seed.stmt, seed.args...); err != nil {
			t.Fatalf("seed progress rows: %v", err)
		}
	}
	got, err := repo.SourceIndexProgress(ctx, tenant, connectionID, "prog_col")
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	want := source.IndexProgress{Items: 2, Indexed: 1, Failed: 1, FailureReason: "connection reset", FailureCount: 1}
	if got != want {
		t.Fatalf("progress = %+v; want %+v", got, want)
	}
}
