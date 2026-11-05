package postgres

import (
	"context"
	"testing"

	"enclava-go/internal/source"
)

func seedUser(t *testing.T, ctx context.Context, db *DB, tenant, id string) {
	t.Helper()
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO users (tenant_id, id, email)
		VALUES ($1, $2, $3)`,
		tenant, id, id+"@test.local"); err != nil {
		t.Fatalf("seed user %s: %v", id, err)
	}
}

func newRepo(t *testing.T) (*DB, *SourceRepo, context.Context) {
	t.Helper()
	db := RequireTestDB(t)
	return db, NewSourceRepo(db), context.Background()
}

// TestSourceRepoObjects covers upsert dedupe, tombstone visibility, parent
// hierarchy and cross-tenant rejection.
func TestSourceRepoObjects(t *testing.T) {
	db, repo, ctx := newRepo(t)
	seedTenant(t, ctx, db, idTenantA)
	seedTenant(t, ctx, db, idTenantB)
	if err := repo.CreateConnection(ctx, source.Connection{
		TenantID: idTenantA, ID: "conn_a", Kind: "github", Name: "gh", CredentialRef: "ref"}); err != nil {
		t.Fatalf("create connection: %v", err)
	}

	obj, err := repo.UpsertObject(ctx, source.Object{
		TenantID: idTenantA, ConnectionID: "conn_a", ID: "obj_parent", Kind: "repo",
		NativeID: "acme/widgets", AuthorizationMode: source.AuthorizationCollection,
		ObservedSeq: 1})
	if err != nil {
		t.Fatalf("upsert parent: %v", err)
	}
	if obj.ID != "obj_parent" {
		t.Fatalf("upsert returned %q", obj.ID)
	}
	// Same identity returns the persisted row — identity fields unmutated.
	again, err := repo.UpsertObject(ctx, source.Object{
		TenantID: idTenantA, ConnectionID: "conn_a", ID: "obj_parent_dup", Kind: "repo",
		NativeID: "acme/widgets", AuthorizationMode: source.AuthorizationOwner,
		OwnerActorID: "user1", ObservedSeq: 5})
	if err != nil {
		t.Fatalf("upsert duplicate: %v", err)
	}
	if again.ID != "obj_parent" || again.AuthorizationMode != source.AuthorizationCollection {
		t.Fatalf("upsert mutated identity: %+v", again)
	}

	// Hierarchy works.
	child, err := repo.UpsertObject(ctx, source.Object{
		TenantID: idTenantA, ConnectionID: "conn_a", ID: "obj_child", Kind: "file",
		NativeID: "acme/widgets/main.go", ParentObjectID: "obj_parent",
		AuthorizationMode: source.AuthorizationCollection, ObservedSeq: 2})
	if err != nil {
		t.Fatalf("upsert child: %v", err)
	}
	if child.ParentObjectID != "obj_parent" {
		t.Fatalf("child parent = %q", child.ParentObjectID)
	}

	// Cross-tenant parent insert fails on the composite FK.
	if _, err := repo.UpsertObject(ctx, source.Object{
		TenantID: idTenantB, ConnectionID: "", ID: "obj_evil", Kind: "file",
		NativeID: "x", ParentObjectID: "obj_parent",
		AuthorizationMode: source.AuthorizationOwner, ObservedSeq: 1}); err == nil {
		t.Fatalf("cross-tenant parent insert was accepted")
	}

	// Versions: monotonic insert, cross-tenant rejected.
	ver := source.ObjectVersion{ObjectID: "obj_parent", ObservedSeq: 1,
		ArtifactRef: "art1", SHA256: "aa", ByteSize: 10, MimeType: "text/plain"}
	if err := repo.InsertObjectVersion(ctx, idTenantA, ver); err != nil {
		t.Fatalf("insert version: %v", err)
	}
	if err := repo.InsertObjectVersion(ctx, idTenantA, ver); err == nil {
		t.Fatalf("non-monotonic observed_seq was accepted")
	}
	if err := repo.InsertObjectVersion(ctx, idTenantB, ver); err == nil {
		t.Fatalf("cross-tenant version insert was accepted")
	}
	latest, err := repo.LatestObjectVersion(ctx, idTenantA, "obj_parent")
	if err != nil {
		t.Fatalf("latest version: %v", err)
	}
	if latest.ObservedSeq != 1 || latest.ArtifactRef != "art1" {
		t.Fatalf("latest version = %+v", latest)
	}

	// Tombstoned object stays queryable but flagged.
	if err := repo.TombstoneObject(ctx, idTenantA, "obj_child"); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	got, err := repo.GetObject(ctx, idTenantA, "obj_child")
	if err != nil {
		t.Fatalf("get tombstoned object: %v", err)
	}
	if got.TombstonedAt == nil {
		t.Fatalf("tombstone flag missing")
	}
	revision := "new-revision"
	if err := repo.ObserveObject(ctx, idTenantA, "obj_child", "obj_parent", &revision); err != nil {
		t.Fatalf("observe tombstoned object: %v", err)
	}
	got, err = repo.GetObject(ctx, idTenantA, "obj_child")
	if err != nil || got.TombstonedAt != nil || got.UpstreamRevision == nil || *got.UpstreamRevision != revision {
		t.Fatalf("observed object=%+v err=%v", got, err)
	}
}

// TestSourceRepoRestrictions proves deny-capability lists and required
// assurance round-trip per subject kind.
func TestSourceRepoRestrictions(t *testing.T) {
	db, repo, ctx := newRepo(t)
	seedTenant(t, ctx, db, idTenantA)
	if err := repo.CreateConnection(ctx, source.Connection{
		TenantID: idTenantA, ID: "conn_a", Kind: "github", Name: "gh", CredentialRef: "ref"}); err != nil {
		t.Fatalf("create connection: %v", err)
	}

	want := source.SourceRestriction{
		TenantID: idTenantA, SubjectKind: source.RestrictionConnection, SubjectID: "conn_a",
		RequiredAssurance: "high", DeniedCapabilities: []string{"public_endpoint", "egress"},
		PolicyLabels: []string{"confidential"},
	}
	if err := repo.SetRestriction(ctx, want); err != nil {
		t.Fatalf("set restriction: %v", err)
	}
	got, err := repo.RestrictionsForSubject(ctx, idTenantA, source.RestrictionConnection, "conn_a")
	if err != nil {
		t.Fatalf("get restrictions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 restriction, got %v", got)
	}
	r := got[0]
	if r.RequiredAssurance != "high" || len(r.DeniedCapabilities) != 2 || len(r.PolicyLabels) != 1 {
		t.Fatalf("restriction round-trip mismatch: %+v", r)
	}
}

// TestCollectionsForObjects proves the authorizer's containing-collection
// read: live items map objects to their collection access rows, removed
// items and foreign tenants are excluded.
func TestCollectionsForObjects(t *testing.T) {
	db, repo, ctx := newRepo(t)
	seedTenant(t, ctx, db, idTenantA)
	seedTenant(t, ctx, db, idTenantB)
	seedUser(t, ctx, db, idTenantA, "user1")
	if err := repo.CreateConnection(ctx, source.Connection{
		TenantID: idTenantA, ID: "conn_a", Kind: "github", Name: "gh", CredentialRef: "ref"}); err != nil {
		t.Fatalf("create connection: %v", err)
	}
	for _, id := range []string{"obj_a", "obj_b", "obj_c"} {
		if _, err := repo.UpsertObject(ctx, source.Object{
			TenantID: idTenantA, ConnectionID: "conn_a", ID: id, Kind: "repo",
			NativeID: "acme/" + id, ObservedSeq: 1}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	collection := func(id string, active bool, audience string) {
		if _, err := db.SQL.ExecContext(ctx, `
			INSERT INTO knowledge_collections
			    (tenant_id, id, name, embedding_model, qdrant_collection, owner_actor_id, is_active, audience)
			VALUES ($1, $2, $2, 'model', 'q_'||$2, 'user1', $3, $4::jsonb)`,
			idTenantA, id, active, audience); err != nil {
			t.Fatalf("collection %s: %v", id, err)
		}
	}
	item := func(coll, obj string, removed bool) {
		if _, err := db.SQL.ExecContext(ctx, `
			INSERT INTO collection_items (tenant_id, id, collection_id, object_id, removed_at)
			VALUES ($1, $2, $3, $4, CASE WHEN $5 THEN now() ELSE NULL END)`,
			idTenantA, "it_"+coll+"_"+obj, coll, obj, removed); err != nil {
			t.Fatalf("item: %v", err)
		}
	}
	collection("coll_a", true, `["u1","*"]`)
	collection("coll_b", false, `[]`)
	item("coll_a", "obj_a", false)
	item("coll_b", "obj_a", false)
	item("coll_a", "obj_b", true)

	got, err := repo.CollectionsForObjects(ctx, idTenantA, []string{"obj_a", "obj_b", "obj_c"})
	if err != nil {
		t.Fatalf("collections for objects: %v", err)
	}
	if len(got["obj_a"]) != 2 {
		t.Fatalf("obj_a collections = %+v", got["obj_a"])
	}
	byID := map[string]source.CollectionAccess{}
	for _, c := range got["obj_a"] {
		byID[c.ID] = c
	}
	if !byID["coll_a"].IsActive || byID["coll_a"].OwnerActorID != "user1" || len(byID["coll_a"].Audience) != 2 {
		t.Fatalf("coll_a access = %+v", byID["coll_a"])
	}
	if byID["coll_b"].IsActive {
		t.Fatalf("coll_b access = %+v", byID["coll_b"])
	}
	if _, ok := got["obj_b"]; ok {
		t.Fatalf("removed item must not map obj_b: %+v", got["obj_b"])
	}
	if _, ok := got["obj_c"]; ok {
		t.Fatalf("collection-less obj_c must be absent: %+v", got["obj_c"])
	}
	other, err := repo.CollectionsForObjects(ctx, idTenantB, []string{"obj_a"})
	if err != nil || len(other) != 0 {
		t.Fatalf("cross-tenant = %+v err=%v", other, err)
	}
}

// TestIngestHandoffExists proves the crash-window query: a matching jobs row
// or an index generation for the observed object version both count as
// durable handoff; unrelated rows do not.
func TestIngestHandoffExists(t *testing.T) {
	db, repo, ctx := newRepo(t)
	seedTenant(t, ctx, db, idTenantA)
	if err := repo.CreateConnection(ctx, source.Connection{
		TenantID: idTenantA, ID: "conn_a", Kind: "github", Name: "gh", CredentialRef: "ref"}); err != nil {
		t.Fatalf("create connection: %v", err)
	}
	if _, err := repo.UpsertObject(ctx, source.Object{
		TenantID: idTenantA, ConnectionID: "conn_a", ID: "obj_a", Kind: "repo",
		NativeID: "acme/widgets", AuthorizationMode: source.AuthorizationCollection,
		ObservedSeq: 1}); err != nil {
		t.Fatalf("upsert object: %v", err)
	}
	if err := repo.InsertObjectVersion(ctx, idTenantA, source.ObjectVersion{
		ObjectID: "obj_a", ObservedSeq: 5, ArtifactRef: "art_a",
		SHA256: "aa", ByteSize: 1, MimeType: "text/plain"}); err != nil {
		t.Fatalf("insert version: %v", err)
	}
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO knowledge_collections (tenant_id, id, name, qdrant_collection)
		VALUES ($1, 'col_a', 'Col A', 'qdr_a')`, idTenantA); err != nil {
		t.Fatalf("insert collection: %v", err)
	}

	assert := func(want bool, note string) {
		got, err := repo.IngestHandoffExists(ctx, idTenantA, "conn_a", "col_a", "obj_a", 5)
		if err != nil || got != want {
			t.Fatalf("%s: handoff=%v err=%v, want %v", note, got, err, want)
		}
	}
	assert(false, "no jobs or generations")

	// A job covering a different observed seq is not handoff for seq 5.
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO jobs (tenant_id, id, operation, status, connection_id,
		                  object_id, observed_revision, idempotency_key,
		                  input_hash, principal, submitter_key, policy)
		VALUES ($1, 'job_other', 'rag.ingest', 'queued', 'conn_a',
		        'obj_a', 4, 'key-other', 'h', '{}', 'sub',
		        '{"collection_id":"col_a"}'::jsonb)`, idTenantA); err != nil {
		t.Fatalf("insert unrelated job: %v", err)
	}
	assert(false, "job for other seq")

	// Terminated jobs (failed/cancelled/deleted) never count as handoff.
	for _, st := range []string{"failed", "cancelled", "deleted"} {
		if _, err := db.SQL.ExecContext(ctx, `
			INSERT INTO jobs (tenant_id, id, operation, status, connection_id,
			                  object_id, observed_revision, idempotency_key,
			                  input_hash, principal, submitter_key, policy)
			VALUES ($1, $2, 'rag.ingest', $3, 'conn_a',
			        'obj_a', 5, $4, 'h', '{}', 'sub',
			        '{"collection_id":"col_a"}'::jsonb)`,
			idTenantA, "job_dead_"+st, st, "key-dead-"+st); err != nil {
			t.Fatalf("insert %s job: %v", st, err)
		}
	}
	assert(false, "dead job statuses")

	// A job covering exactly (connection, object, seq, collection) is handoff.
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO jobs (tenant_id, id, operation, status, connection_id,
		                  object_id, observed_revision, idempotency_key,
		                  input_hash, principal, submitter_key, policy)
		VALUES ($1, 'job_seq5', 'rag.ingest', 'queued', 'conn_a',
		        'obj_a', 5, 'key-seq5', 'h', '{}', 'sub',
		        '{"collection_id":"col_a"}'::jsonb)`, idTenantA); err != nil {
		t.Fatalf("insert matching job: %v", err)
	}
	assert(true, "job covers seq 5")

	// A job targeting a different collection is not handoff for col_b.
	if got, err := repo.IngestHandoffExists(ctx, idTenantA, "conn_a", "col_b", "obj_a", 5); err != nil || got {
		t.Fatalf("job for col_a must not count for col_b: handoff=%v err=%v", got, err)
	}
}

// TestIngestHandoffExistsGeneration proves the generation half: a generation
// for the object version in the target collection is durable handoff even
// with no jobs row, and only in that collection.
func TestIngestHandoffExistsGeneration(t *testing.T) {
	db, repo, ctx := newRepo(t)
	seedTenant(t, ctx, db, idTenantA)
	if err := repo.CreateConnection(ctx, source.Connection{
		TenantID: idTenantA, ID: "conn_a", Kind: "github", Name: "gh", CredentialRef: "ref"}); err != nil {
		t.Fatalf("create connection: %v", err)
	}
	if _, err := repo.UpsertObject(ctx, source.Object{
		TenantID: idTenantA, ConnectionID: "conn_a", ID: "obj_a", Kind: "repo",
		NativeID: "acme/widgets", AuthorizationMode: source.AuthorizationCollection,
		ObservedSeq: 1}); err != nil {
		t.Fatalf("upsert object: %v", err)
	}
	if err := repo.InsertObjectVersion(ctx, idTenantA, source.ObjectVersion{
		ObjectID: "obj_a", ObservedSeq: 3, ArtifactRef: "art_a",
		SHA256: "aa", ByteSize: 1, MimeType: "text/plain"}); err != nil {
		t.Fatalf("insert version: %v", err)
	}
	for _, col := range []string{"col_a", "col_b"} {
		if _, err := db.SQL.ExecContext(ctx, `
			INSERT INTO knowledge_collections (tenant_id, id, name, qdrant_collection)
			VALUES ($1, $2, $2, 'qdr_' || $2)`, idTenantA, col); err != nil {
			t.Fatalf("insert collection %s: %v", col, err)
		}
	}
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO index_generations (tenant_id, id, collection_id,
		                               object_version_id, generation, state)
		VALUES ($1, 'gen_a', 'col_a', 'obj_a#3', 1, 'published')`, idTenantA); err != nil {
		t.Fatalf("insert generation: %v", err)
	}

	exists, err := repo.IngestHandoffExists(ctx, idTenantA, "conn_a", "col_a", "obj_a", 3)
	if err != nil || !exists {
		t.Fatalf("generation in target collection: handoff=%v err=%v", exists, err)
	}
	if exists, err := repo.IngestHandoffExists(ctx, idTenantA, "conn_a", "col_b", "obj_a", 3); err != nil || exists {
		t.Fatalf("generation in other collection must not count: handoff=%v err=%v", exists, err)
	}
	if exists, err := repo.IngestHandoffExists(ctx, idTenantA, "conn_a", "col_a", "obj_a", 4); err != nil || exists {
		t.Fatalf("generation covers seq 3 only: handoff=%v err=%v", exists, err)
	}
}

// TestIngestFailed mirrors the document "failed" status for the newest
// generation: a failed embed job on a staging generation counts, an
// interrupted ('unknown') job counts only while its generation stays
// unpublished, a newer published generation clears it, and other
// collections never count.
func TestIngestFailed(t *testing.T) {
	db, repo, ctx := newRepo(t)
	seedTenant(t, ctx, db, idTenantA)
	if err := repo.CreateConnection(ctx, source.Connection{
		TenantID: idTenantA, ID: "conn_a", Kind: "slack", Name: "slack", CredentialRef: "ref"}); err != nil {
		t.Fatalf("create connection: %v", err)
	}
	if _, err := repo.UpsertObject(ctx, source.Object{
		TenantID: idTenantA, ConnectionID: "conn_a", ID: "obj_a", Kind: "message",
		NativeID: "slack:C1:1.0", AuthorizationMode: source.AuthorizationCollection, ObservedSeq: 1}); err != nil {
		t.Fatalf("upsert object: %v", err)
	}
	if err := repo.InsertObjectVersion(ctx, idTenantA, source.ObjectVersion{
		ObjectID: "obj_a", ObservedSeq: 2, ArtifactRef: "art_a", SHA256: "aa", ByteSize: 1, MimeType: "text/plain"}); err != nil {
		t.Fatalf("insert version: %v", err)
	}
	for _, col := range []string{"col_a", "col_b"} {
		if _, err := db.SQL.ExecContext(ctx, `
			INSERT INTO knowledge_collections (tenant_id, id, name, qdrant_collection)
			VALUES ($1, $2, $2, 'qdr_' || $2)`, idTenantA, col); err != nil {
			t.Fatalf("insert collection %s: %v", col, err)
		}
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.SQL.ExecContext(ctx, query, append([]any{idTenantA}, args...)...); err != nil {
			t.Fatal(err)
		}
	}
	assert := func(collection string, want bool, note string) {
		t.Helper()
		got, err := repo.IngestFailed(ctx, idTenantA, collection, "obj_a", 2)
		if err != nil || got != want {
			t.Fatalf("%s: failed=%v err=%v, want %v", note, got, err, want)
		}
	}
	assert("col_a", false, "no generation")

	exec(`INSERT INTO index_generations (tenant_id, id, collection_id, object_version_id, generation, state)
		VALUES ($1, 'gen_1', 'col_a', 'obj_a#2', 1, 'staging')`)
	exec(`INSERT INTO jobs (tenant_id, id, operation, status, connection_id, object_id, observed_revision,
		                  idempotency_key, input_ref, input_hash, principal, submitter_key, policy)
		VALUES ($1, 'job_embed_1', 'rag.embed', 'failed', 'conn_a', 'obj_a', 2,
		        'key-embed-1', 'obj_a#2', 'h', '{}', 'sub', '{"generation":"1"}'::jsonb)`)
	assert("col_a", true, "staging generation whose embed job failed")
	assert("col_b", false, "failure in another collection")

	exec(`INSERT INTO index_generations (tenant_id, id, collection_id, object_version_id, generation, state)
		VALUES ($1, 'gen_2', 'col_a', 'obj_a#2', 2, 'published')`)
	assert("col_a", false, "newer published generation")

	exec(`INSERT INTO index_generations (tenant_id, id, collection_id, object_version_id, generation, state)
		VALUES ($1, 'gen_3', 'col_a', 'obj_a#2', 3, 'failed')`)
	assert("col_a", true, "newest generation failed")

	exec(`INSERT INTO index_generations (tenant_id, id, collection_id, object_version_id, generation, state)
		VALUES ($1, 'gen_4', 'col_a', 'obj_a#2', 4, 'staging')`)
	exec(`INSERT INTO jobs (tenant_id, id, operation, status, connection_id, object_id, observed_revision,
		                  idempotency_key, input_ref, input_hash, principal, submitter_key, policy)
		VALUES ($1, 'job_embed_4', 'rag.embed', 'unknown', 'conn_a', 'obj_a', 2,
		        'key-embed-4', 'obj_a#2', 'h', '{}', 'sub', '{"generation":"4"}'::jsonb)`)
	assert("col_a", true, "staging generation whose embed job was interrupted")

	exec(`INSERT INTO index_generations (tenant_id, id, collection_id, object_version_id, generation, state)
		VALUES ($1, 'gen_5', 'col_a', 'obj_a#2', 5, 'published')`)
	exec(`INSERT INTO jobs (tenant_id, id, operation, status, connection_id, object_id, observed_revision,
		                  idempotency_key, input_ref, input_hash, principal, submitter_key, policy)
		VALUES ($1, 'job_embed_5', 'rag.embed', 'unknown', 'conn_a', 'obj_a', 2,
		        'key-embed-5', 'obj_a#2', 'h', '{}', 'sub', '{"generation":"5"}'::jsonb)`)
	assert("col_a", false, "published generation clears an interrupted job")
}
