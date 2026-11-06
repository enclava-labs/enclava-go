package postgres

import (
	"context"
	"testing"
	"time"
)

// TestDeleteConnectionRemovesSyncedRows proves deleting a synced source is
// atomic: generations, chunks, parsed evidence and collection membership of
// its objects go with it, its artifacts tombstone for the reconcile sweep,
// and queued work is cancelled — no FK violation, no leftovers.
func TestDeleteConnectionRemovesSyncedRows(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()

	seedTenant(t, ctx, db, idTenantA)
	seedConnection(t, ctx, db, idTenantA, "conn_del", "github")
	seedChunkProfile(t, ctx, db, idTenantA, "cp_v1", "fam_1", 1)

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.SQL.ExecContext(ctx, query, append([]any{idTenantA}, args...)...); err != nil {
			t.Fatal(err)
		}
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := db.SQL.QueryRowContext(ctx, query, append([]any{idTenantA}, args...)...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Two synced objects; obj_2 carries two observed versions.
	for _, objID := range []string{"obj_1", "obj_2"} {
		exec(`INSERT INTO source_objects (tenant_id, id, connection_id, kind, native_id,
		                                 authorization_mode, observed_seq)
		      VALUES ($1, $2, 'conn_del', 'repo', 'acme/' || $2, 'collection', 2)`, objID)
	}
	exec(`INSERT INTO source_object_versions (tenant_id, id, object_id, observed_seq,
	                                          artifact_ref, content_sha256, byte_size)
	      VALUES ($1, 'ver_1', 'obj_1', 1, 'art_1', 'sha1', 10)`)
	exec(`INSERT INTO source_object_versions (tenant_id, id, object_id, observed_seq,
	                                          artifact_ref, content_sha256, byte_size)
	      VALUES ($1, 'ver_2', 'obj_2', 1, 'art_2', 'sha2', 10),
	             ($1, 'ver_3', 'obj_2', 2, 'art_2', 'sha3', 10)`)

	exec(`INSERT INTO knowledge_collections (tenant_id, id, name, qdrant_collection)
	      VALUES ($1, 'col_a', 'docs', 'qdrant_a')`)
	for _, objID := range []string{"obj_1", "obj_2"} {
		exec(`INSERT INTO collection_items (tenant_id, id, collection_id, object_id)
		      VALUES ($1, 'ci_' || $2, 'col_a', $2)`, objID)
	}

	// A published generation plus its pending reconciliation and chunk.
	exec(`INSERT INTO index_generations (tenant_id, id, collection_id, object_version_id,
	                                     generation, state, qdrant_collection)
	      VALUES ($1, 'gen_1', 'col_a', 'ver_1', 1, 'published', 'qdrant_a')`)
	exec(`INSERT INTO index_reconciliation (tenant_id, collection_id, object_version_id,
	                                        generation, fence_token, state)
	      VALUES ($1, 'col_a', 'ver_1', 1, 1, 'pending')`)
	exec(`INSERT INTO chunks (tenant_id, id, object_version_id, chunk_profile_version_id,
	                          collection_id, ordinal, text, byte_start, byte_end)
	      VALUES ($1, 'chk_1', 'ver_1', 'cp_v1', 'col_a', 0, 'body', 0, 4)`)

	// Parsed evidence for the same version.
	exec(`INSERT INTO parsed_documents (tenant_id, id, object_version_id, parser, parser_version)
	      VALUES ($1, 'pd_1', 'ver_1', 'text', 'v1')`)
	exec(`INSERT INTO blocks (tenant_id, id, parsed_document_id, ordinal, kind, parent_id,
	                          text, byte_start, byte_end)
	      VALUES ($1, 'blk_1', 'pd_1', 0, 'paragraph', NULL, 'body', 0, 4),
	             ($1, 'blk_2', 'pd_1', 1, 'paragraph', 'blk_1', 'more', 4, 8)`)

	// Evidence row (cascades with the object/version) and the artifact the
	// version's artifact_ref names.
	exec(`INSERT INTO rag_evidence (tenant_id, id, collection_id, object_id,
	                                object_version_id, observed_seq, generation, chunk_id)
	      VALUES ($1, 'ev_1', 'col_a', 'obj_1', 'ver_1', 1, 1, 'chk_1')`)
	exec(`INSERT INTO artifacts (tenant_id, id, kind, sha256, byte_size, state, storage_path)
	      VALUES ($1, 'art_1', 'original', 'sha1', 10, 'final', 'x/y')`)

	// Queued work bound to the connection must not survive it — including
	// embed jobs, which carry only the object.
	exec(`INSERT INTO jobs (tenant_id, id, operation, status, connection_id,
	                        idempotency_key, input_ref, input_hash, principal, submitter_key, policy)
	      VALUES ($1, 'job_sync', 'connector.sync', 'queued', 'conn_del',
	              'key-sync', 'ver_1', 'h', '{}', 'sub', '{}'::jsonb)`)
	exec(`INSERT INTO jobs (tenant_id, id, operation, status, object_id,
	                        idempotency_key, input_ref, input_hash, principal, submitter_key, policy)
	      VALUES ($1, 'job_embed', 'rag.embed', 'queued', 'obj_1',
	              'key-embed', 'ver_1', 'h', '{}', 'sub', '{"generation":"1"}'::jsonb)`)

	if err := NewSourceRepo(db).DeleteConnection(ctx, idTenantA, "conn_del"); err != nil {
		t.Fatalf("delete synced connection: %v", err)
	}

	for table, query := range map[string]string{
		"source_objects":         `SELECT count(*) FROM source_objects WHERE tenant_id = $1 AND connection_id = 'conn_del'`,
		"source_object_versions": `SELECT count(*) FROM source_object_versions WHERE tenant_id = $1 AND object_id IN ('obj_1','obj_2')`,
		"index_generations":      `SELECT count(*) FROM index_generations WHERE tenant_id = $1 AND object_version_id IN ('ver_1','ver_2','ver_3')`,
		"index_reconciliation":   `SELECT count(*) FROM index_reconciliation WHERE tenant_id = $1 AND object_version_id IN ('ver_1','ver_2','ver_3')`,
		"chunks":                 `SELECT count(*) FROM chunks WHERE tenant_id = $1 AND object_version_id IN ('ver_1','ver_2','ver_3')`,
		"parsed_documents":       `SELECT count(*) FROM parsed_documents WHERE tenant_id = $1 AND object_version_id IN ('ver_1','ver_2','ver_3')`,
		"blocks":                 `SELECT count(*) FROM blocks WHERE tenant_id = $1 AND parsed_document_id = 'pd_1'`,
		"collection_items":       `SELECT count(*) FROM collection_items WHERE tenant_id = $1 AND object_id IN ('obj_1','obj_2')`,
		"rag_evidence":           `SELECT count(*) FROM rag_evidence WHERE tenant_id = $1 AND object_id IN ('obj_1','obj_2')`,
		"connections":            `SELECT count(*) FROM connections WHERE tenant_id = $1 AND id = 'conn_del'`,
	} {
		if n := count(query); n != 0 {
			t.Fatalf("%s rows left after delete: %d", table, n)
		}
	}

	var state, reason string
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT state, tombstone_reason FROM artifacts WHERE tenant_id = $1 AND id = 'art_1'`,
		idTenantA).Scan(&state, &reason); err != nil {
		t.Fatalf("read artifact row: %v", err)
	}
	if state != "tombstoned" || reason != "deleted" {
		t.Fatalf("artifact must be tombstoned(deleted), got %s/%s", state, reason)
	}

	for _, jobID := range []string{"job_sync", "job_embed"} {
		var jobStatus string
		if err := db.SQL.QueryRowContext(ctx,
			`SELECT status FROM jobs WHERE tenant_id = $1 AND id = $2`,
			idTenantA, jobID).Scan(&jobStatus); err != nil {
			t.Fatalf("read job row %s: %v", jobID, err)
		}
		if jobStatus != "cancelled" {
			t.Fatalf("queued job %s must be cancelled, got %s", jobID, jobStatus)
		}
	}
}

// TestDeleteConnectionWaitsForConcurrentWriters: a sync or embed still
// writing (here an uncommitted generation for one of the source's versions)
// must not make the delete fail on a foreign key — the delete waits for the
// writer and removes what it wrote.
func TestDeleteConnectionWaitsForConcurrentWriters(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	seedTenant(t, ctx, db, idTenantA)
	seedConnection(t, ctx, db, idTenantA, "conn_race", "slack")
	for _, q := range []string{
		`INSERT INTO source_objects (tenant_id, id, connection_id, kind, native_id, authorization_mode, observed_seq)
		 VALUES ($1, 'obj_race', 'conn_race', 'message', 'slack:race', 'collection', 1)`,
		`INSERT INTO source_object_versions (tenant_id, id, object_id, observed_seq, artifact_ref, content_sha256, byte_size)
		 VALUES ($1, 'ver_race', 'obj_race', 1, 'art_race', 'sha', 10)`,
		`INSERT INTO knowledge_collections (tenant_id, id, name, qdrant_collection)
		 VALUES ($1, 'col_race', 'race', 'qdrant_race')`,
	} {
		if _, err := db.SQL.ExecContext(ctx, q, idTenantA); err != nil {
			t.Fatal(err)
		}
	}

	writer, err := db.SQL.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback() //nolint:errcheck // committed below on the happy path
	if _, err := writer.ExecContext(ctx, `INSERT INTO index_generations (tenant_id, id, collection_id, object_version_id, generation, state)
		VALUES ($1, 'gen_race', 'col_race', 'ver_race', 1, 'staging')`, idTenantA); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- NewSourceRepo(db).DeleteConnection(ctx, idTenantA, "conn_race") }()
	select {
	case err := <-done:
		t.Fatalf("delete finished while a writer still held its rows: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("delete after the concurrent write: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delete never finished")
	}
	var left int
	if err := db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM index_generations WHERE tenant_id = $1 AND id = 'gen_race'`, idTenantA).Scan(&left); err != nil || left != 0 {
		t.Fatalf("generation written during the delete survived: %d, %v", left, err)
	}
}
