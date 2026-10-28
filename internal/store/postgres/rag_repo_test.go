package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestRAGRepoCollectionsTenantScoped(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	tenantA := testdb.SeedTenant(t, db, "a")
	tenantB := testdb.SeedTenant(t, db, "b")
	repo := postgres.NewRAGRepo(db)

	c := rag.Collection{
		TenantID:         tenantA,
		ID:               "ragcol_test",
		Name:             "Docs",
		Description:      "d",
		OwnerActorID:     "user_a",
		QdrantCollection: "rag_ragcol_test",
		EmbeddingModel:   "embed-test",
		ChunkSize:        500,
		ChunkOverlap:     50,
		ACL:              map[string]any{"public": true},
		Audience:         []string{"*"},
		Metadata:         map[string]any{"team": "search"},
		IsActive:         true,
	}
	if err := repo.InsertCollection(ctx, c); err != nil {
		t.Fatalf("insert collection: %v", err)
	}
	got, err := repo.GetCollection(ctx, tenantA, c.ID)
	if err != nil {
		t.Fatalf("get collection: %v", err)
	}
	if got.Name != "Docs" || got.ChunkSize != 500 || got.ChunkOverlap != 50 ||
		got.EmbeddingModel != "embed-test" || got.Metadata["team"] != "search" ||
		got.ACL["public"] != true || len(got.Audience) != 1 || got.Audience[0] != "*" {
		t.Fatalf("collection round-trip mismatch: %#v", got)
	}
	if _, err := repo.GetCollection(ctx, tenantB, c.ID); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("expected cross-tenant deny, got %v", err)
	}
	list, err := repo.ListCollections(ctx, tenantA)
	if err != nil || len(list) != 1 {
		t.Fatalf("list collections: %v %#v", err, list)
	}
	got.Name = "Renamed"
	got.IsActive = false
	if err := repo.UpdateCollection(ctx, got); err != nil {
		t.Fatalf("update collection: %v", err)
	}
	got2, _ := repo.GetCollection(ctx, tenantA, c.ID)
	if got2.Name != "Renamed" || got2.IsActive {
		t.Fatalf("update not applied: %#v", got2)
	}
	if err := repo.DeleteCollection(ctx, tenantB, c.ID); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("expected cross-tenant delete deny, got %v", err)
	}
	if err := repo.DeleteCollection(ctx, tenantA, c.ID); err != nil {
		t.Fatalf("delete collection: %v", err)
	}
}

func TestRAGRepoChunkProfileVersions(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	tenant := testdb.SeedTenant(t, db, "cp")
	repo := postgres.NewRAGRepo(db)

	id1, err := repo.EnsureChunkProfile(ctx, tenant, "rag-col", 100, 10)
	if err != nil {
		t.Fatalf("ensure profile: %v", err)
	}
	again, err := repo.EnsureChunkProfile(ctx, tenant, "rag-col", 100, 10)
	if err != nil || again != id1 {
		t.Fatalf("same params must return same immutable row: %s vs %s", again, id1)
	}
	id2, err := repo.EnsureChunkProfile(ctx, tenant, "rag-col", 200, 10)
	if err != nil {
		t.Fatalf("ensure profile v2: %v", err)
	}
	if id2 == id1 {
		t.Fatal("changed params must create a new version row")
	}
	var version int
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT version FROM chunk_profiles WHERE tenant_id = $1 AND id = $2`,
		tenant, id2).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != 2 {
		t.Fatalf("expected version 2, got %d", version)
	}
}

func TestRAGRepoCollectionChunkReplacementPreservesSharedVersionChunks(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	tenant := testdb.SeedTenant(t, db, "docs")
	repo := postgres.NewRAGRepo(db)

	if err := repo.InsertCollection(ctx, rag.Collection{
		TenantID: tenant, ID: "ragcol_1", Name: "C", QdrantCollection: "rag_1",
		EmbeddingModel: "m", ChunkSize: 100, ChunkOverlap: 10, IsActive: true,
	}); err != nil {
		t.Fatalf("insert collection: %v", err)
	}
	obj, err := repo.UpsertObject(ctx, source.Object{
		TenantID: tenant, ID: "obj_1", Kind: "upload", NativeID: "up_1",
		AuthorizationMode: source.AuthorizationAudience, OwnerActorID: "user_1",
		Audience: []string{"user_1"},
	})
	if err != nil {
		t.Fatalf("upsert object: %v", err)
	}
	if err := repo.InsertObjectVersion(ctx, tenant, source.ObjectVersion{
		ObjectID: obj.ID, ObservedSeq: 1, ArtifactRef: "art_1", SHA256: "abc",
		ByteSize: 5, MimeType: "text/plain", OriginalFilename: "a.txt",
		Provenance: `{"source_uri":"file:///a.txt","metadata":{"k":"v"}}`,
	}); err != nil {
		t.Fatalf("insert version: %v", err)
	}
	if err := repo.AddCollectionItem(ctx, tenant, "ragcol_1", obj.ID, "user_1"); err != nil {
		t.Fatalf("add item: %v", err)
	}
	doc, err := repo.GetDocument(ctx, tenant, obj.ID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	if doc.Status != rag.DocumentStatusProcessing || doc.FileName != "a.txt" ||
		doc.SourceURI != "file:///a.txt" || doc.Metadata["k"] != "v" {
		t.Fatalf("document projection mismatch: %#v", doc)
	}
	parsed := source.Document{
		Ref: source.ObjectRef{TenantID: tenant, ObjectID: obj.ID, ObservedSeq: 1},
		Blocks: []source.Block{
			{ID: "blk_1", Ordinal: 0, Kind: "heading", Text: "Title", ByteStart: 0, ByteEnd: 5, Locator: source.Locator{HeadingLevel: 1}},
			{ID: "blk_2", Ordinal: 1, Kind: "paragraph", ParentID: "blk_1", Text: "hello", ByteStart: 6, ByteEnd: 11},
		},
	}
	if err := repo.SaveParsedDocument(ctx, "rag-structured-v1", parsed); err != nil {
		t.Fatalf("save parsed document: %v", err)
	}
	var blocks int
	if err := db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM blocks WHERE tenant_id = $1`, tenant).Scan(&blocks); err != nil || blocks != 2 {
		t.Fatalf("parsed blocks = %d, %v", blocks, err)
	}
	gen, err := repo.NextGeneration(ctx, tenant, "ragcol_1", doc.ObjectVersionID)
	if err != nil || gen != 1 {
		t.Fatalf("next generation = %d, %v", gen, err)
	}
	chunks := []rag.DocumentChunk{{
		ID: "chk_1", DocumentID: obj.ID, CollectionID: "ragcol_1",
		ChunkIndex: 0, Content: "hello", TokenCount: 1, ByteStart: 6, ByteEnd: 11,
		SpanRefs: []source.Span{{Ref: parsed.Ref, BlockID: "blk_2", ByteStart: 0, ByteEnd: 5}},
	}}
	if err := repo.ReplaceVersionChunksAndStage(ctx, tenant, "ragcol_1", obj.ID, doc.ObjectVersionID, 1, gen, "", chunks); err == nil {
		t.Fatal("expected chunk insert to fail without a bound profile")
	}
	profileID, err := repo.EnsureChunkProfile(ctx, tenant, "rag-ragcol_1-structured", 100, 10)
	if err != nil {
		t.Fatalf("ensure profile: %v", err)
	}
	if err := repo.ReplaceVersionChunksAndStage(ctx, tenant, "ragcol_1", obj.ID, doc.ObjectVersionID, 1, gen, profileID, chunks); err != nil {
		t.Fatalf("stage chunks+generation: %v", err)
	}
	if n, err := repo.ChunkCountForVersion(ctx, tenant, doc.ObjectVersionID); err != nil || n != 1 {
		t.Fatalf("chunk count = %d, %v", n, err)
	}
	roundTrip, err := repo.ListChunks(ctx, tenant, obj.ID, doc.ObjectVersionID)
	if err != nil || len(roundTrip) != 1 || roundTrip[0].ByteStart != 6 || roundTrip[0].ByteEnd != 11 ||
		len(roundTrip[0].SpanRefs) != 1 || roundTrip[0].SpanRefs[0].BlockID != "blk_2" {
		t.Fatalf("chunk spans roundtrip: %#v, %v", roundTrip, err)
	}
	if err := repo.MarkGenerationState(ctx, tenant, "ragcol_1", doc.ObjectVersionID, gen, "published"); err != nil {
		t.Fatalf("publish generation: %v", err)
	}
	doc, err = repo.GetDocument(ctx, tenant, obj.ID)
	if err != nil {
		t.Fatalf("get document after publish: %v", err)
	}
	if doc.Status != rag.DocumentStatusReady || doc.QdrantPointCount != 1 {
		t.Fatalf("expected ready doc with 1 point, got %#v", doc)
	}
	listed, err := repo.ListDocuments(ctx, tenant, "ragcol_1", rag.DocumentStatusAll, 10, 0)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list documents: %v %#v", err, listed)
	}
	if err := repo.InsertCollection(ctx, rag.Collection{
		TenantID: tenant, ID: "ragcol_2", Name: "Other", QdrantCollection: "rag_2",
		EmbeddingModel: "m", ChunkSize: 100, ChunkOverlap: 10, IsActive: true,
	}); err != nil {
		t.Fatalf("insert second collection: %v", err)
	}
	profileB, err := repo.EnsureChunkProfile(ctx, tenant, "rag-ragcol_2-structured", 100, 10)
	if err != nil {
		t.Fatalf("ensure second collection profile: %v", err)
	}
	chunksB := []rag.DocumentChunk{{ID: "chk_b", DocumentID: obj.ID, CollectionID: "ragcol_2", ChunkIndex: 0, Content: "other collection"}}
	if err := repo.ReplaceVersionChunksAndStage(ctx, tenant, "ragcol_2", obj.ID, doc.ObjectVersionID, 1, 1, profileB, chunksB); err != nil {
		t.Fatalf("stage second collection chunks: %v", err)
	}
	gen, err = repo.NextGeneration(ctx, tenant, "ragcol_1", doc.ObjectVersionID)
	if err != nil || gen != 2 {
		t.Fatalf("next replacement generation = %d, %v", gen, err)
	}
	chunksA2 := []rag.DocumentChunk{{ID: "chk_a2", DocumentID: obj.ID, CollectionID: "ragcol_1", ChunkIndex: 0, Content: "replacement"}}
	if err := repo.ReplaceVersionChunksAndStage(ctx, tenant, "ragcol_1", obj.ID, doc.ObjectVersionID, 1, gen, profileID, chunksA2); err != nil {
		t.Fatalf("replace first collection chunks: %v", err)
	}
	var remaining int
	if err := db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM chunks WHERE tenant_id = $1 AND object_version_id = $2 AND id = 'chk_b'`, tenant, doc.ObjectVersionID).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("replacing first collection deleted the other collection's chunk: count=%d err=%v", remaining, err)
	}
}

// seedProjectionFixture builds one tenant with a collection, an audience
// object+version, parsed blocks with real locators, a published generation
// with one span-backed chunk and a saved evidence row.
func seedProjectionFixture(t *testing.T, db *postgres.DB, repo *postgres.RAGRepo, tenant, collectionID string) (objectID, versionID string) {
	t.Helper()
	ctx := context.Background()
	if err := repo.InsertCollection(ctx, rag.Collection{
		TenantID: tenant, ID: collectionID, Name: "C", OwnerActorID: "user_1",
		QdrantCollection: "rag_" + collectionID, EmbeddingModel: "m",
		ChunkSize: 100, ChunkOverlap: 10, IsActive: true,
	}); err != nil {
		t.Fatalf("insert collection: %v", err)
	}
	obj, err := repo.UpsertObject(ctx, source.Object{
		TenantID: tenant, ID: "obj_proj", Kind: "upload", NativeID: "up_proj",
		AuthorizationMode: source.AuthorizationAudience, OwnerActorID: "user_1",
		Audience: []string{"user_1"},
	})
	if err != nil {
		t.Fatalf("upsert object: %v", err)
	}
	if err := repo.InsertObjectVersion(ctx, tenant, source.ObjectVersion{
		ObjectID: obj.ID, ObservedSeq: 1, ArtifactRef: "art_proj", SHA256: "sha_proj",
		ByteSize: 11, MimeType: "text/plain", OriginalFilename: "proj.txt",
		Provenance: `{"source_uri":"file:///proj.txt","metadata":{"team":"core"}}`,
	}); err != nil {
		t.Fatalf("insert version: %v", err)
	}
	if err := repo.AddCollectionItem(ctx, tenant, collectionID, obj.ID, "user_1"); err != nil {
		t.Fatalf("add item: %v", err)
	}
	parsed := source.Document{
		Ref: source.ObjectRef{TenantID: tenant, ObjectID: obj.ID, ObservedSeq: 1},
		Blocks: []source.Block{
			{ID: "blk_h1", Ordinal: 0, Kind: "heading", Text: "Intro", ByteStart: 0, ByteEnd: 5, Locator: source.Locator{HeadingLevel: 1, Section: "Intro"}},
			{ID: "blk_p1", Ordinal: 1, Kind: "paragraph", ParentID: "blk_h1", Text: "hello", ByteStart: 6, ByteEnd: 11, Locator: source.Locator{Page: new(3)}},
		},
	}
	if err := repo.SaveParsedDocument(ctx, "rag-structured-v1", parsed); err != nil {
		t.Fatalf("save parsed document: %v", err)
	}
	return obj.ID, fmt.Sprintf("%s#%d", obj.ID, 1)
}

func stageChunks(t *testing.T, repo *postgres.RAGRepo, tenant, collectionID, objectID, versionID string, generation int64, chunkIDs ...string) {
	t.Helper()
	profileID, err := repo.EnsureChunkProfile(context.Background(), tenant, "rag-"+collectionID+"-structured", 100, 10)
	if err != nil {
		t.Fatalf("ensure profile: %v", err)
	}
	chunks := make([]rag.DocumentChunk, 0, len(chunkIDs))
	for i, id := range chunkIDs {
		chunks = append(chunks, rag.DocumentChunk{
			ID: id, DocumentID: objectID, CollectionID: collectionID, Generation: generation,
			ChunkIndex: i, Content: fmt.Sprintf("chunk %s", id), TokenCount: 2,
			HeaderPath:    "Intro",
			SpanRefs:      []source.Span{{Ref: source.ObjectRef{TenantID: tenant, ObjectID: objectID, ObservedSeq: 1}, BlockID: "blk_p1", ByteStart: 0, ByteEnd: 5}},
			QdrantPointID: id + "_pt",
		})
	}
	if err := repo.ReplaceVersionChunksAndStage(context.Background(), tenant, collectionID, objectID, versionID, 1, generation, profileID, chunks); err != nil {
		t.Fatalf("stage chunks: %v", err)
	}
}

func TestRAGRepoDocumentProjectionTruthAndIdentity(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	tenant := testdb.SeedTenant(t, db, "proj")
	repo := postgres.NewRAGRepo(db)
	objectID, versionID := seedProjectionFixture(t, db, repo, tenant, "ragcol_proj")

	// Generation 1 published with one chunk: ready with a generation-scoped count.
	stageChunks(t, repo, tenant, "ragcol_proj", objectID, versionID, 1, "chk_p1")
	if err := repo.MarkGenerationState(ctx, tenant, "ragcol_proj", versionID, 1, "published"); err != nil {
		t.Fatalf("publish generation 1: %v", err)
	}
	doc, err := repo.GetDocument(ctx, tenant, objectID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	if doc.Status != rag.DocumentStatusReady || doc.QdrantPointCount != 1 || doc.FileName != "proj.txt" || doc.Metadata["team"] != "core" {
		t.Fatalf("published projection mismatch: %#v", doc)
	}

	// Chunk identity: DocumentID is the owning object id, never the version
	// row id; HeaderPath persists from materialization.
	chunks, err := repo.ListGenerationChunks(ctx, tenant, "ragcol_proj", versionID, 1)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("list generation chunks: %v %#v", err, chunks)
	}
	if chunks[0].DocumentID != objectID {
		t.Fatalf("chunk DocumentID = %q, want object id %q", chunks[0].DocumentID, objectID)
	}
	if chunks[0].HeaderPath != "Intro" || len(chunks[0].SpanRefs) != 1 || chunks[0].SpanRefs[0].BlockID != "blk_p1" {
		t.Fatalf("chunk header/spans roundtrip mismatch: %#v", chunks[0])
	}

	// ReadEvidence resolves a real current passage end-to-end; ResolveEvidence
	// fills Locations from the retained block locators.
	if err := repo.SaveEvidence(ctx, rag.EvidenceRecord{
		TenantID: tenant, ID: "ev_proj", CollectionID: "ragcol_proj", ObjectID: objectID,
		ObjectVersionID: versionID, ObservedSeq: 1, Generation: 1, ChunkID: "chk_p1",
		SpanRefs: []source.Span{{Ref: source.ObjectRef{TenantID: tenant, ObjectID: objectID, ObservedSeq: 1}, BlockID: "blk_p1", ByteStart: 0, ByteEnd: 5}},
	}); err != nil {
		t.Fatalf("save evidence: %v", err)
	}
	locatorMap, err := repo.ListBlockLocators(ctx, tenant, []string{"blk_p1", "blk_missing"})
	if err != nil || len(locatorMap) != 1 {
		t.Fatalf("list block locators: %v %#v", err, locatorMap)
	}
	if locatorMap["blk_p1"].Page == nil || *locatorMap["blk_p1"].Page != 3 {
		t.Fatalf("block locator page mismatch: %#v", locatorMap["blk_p1"])
	}
	svc := rag.NewService(repo, nil, nil, "", nil, nil)
	p := auth.Principal{TenantID: tenant, ActorID: "user_1", UserID: "user_1", ActorKind: auth.ActorKindUser}
	text, err := svc.ReadEvidence(ctx, p, "ev_proj")
	if err != nil {
		t.Fatalf("read evidence: %v", err)
	}
	if text.DocumentID != objectID || text.Content != "chunk chk_p1" || len(text.Spans) != 1 {
		t.Fatalf("passage mismatch: %#v", text)
	}
	resolutions, err := svc.ResolveEvidence(ctx, p, []string{"ev_proj"})
	if err != nil || len(resolutions) != 1 {
		t.Fatalf("resolve evidence: %v %#v", err, resolutions)
	}
	if resolutions[0].Status != "current" || len(resolutions[0].Locations) != 1 ||
		resolutions[0].Locations[0].Page == nil || *resolutions[0].Locations[0].Page != 3 {
		t.Fatalf("resolved locations mismatch: %#v", resolutions[0])
	}

	// A newer staged generation becomes the projected state; the chunk count
	// spans only that generation.
	stageChunks(t, repo, tenant, "ragcol_proj", objectID, versionID, 2, "chk_p2a", "chk_p2b")
	doc, err = repo.GetDocument(ctx, tenant, objectID)
	if err != nil {
		t.Fatalf("get staged document: %v", err)
	}
	if doc.Status != rag.DocumentStatusProcessing || doc.QdrantPointCount != 2 {
		t.Fatalf("staged projection mismatch: %#v", doc)
	}
	if err := repo.MarkGenerationState(ctx, tenant, "ragcol_proj", versionID, 2, "published"); err != nil {
		t.Fatalf("publish generation 2: %v", err)
	}
	if err := repo.SaveEvidence(ctx, rag.EvidenceRecord{
		TenantID: tenant, ID: "ev_proj2", CollectionID: "ragcol_proj", ObjectID: objectID,
		ObjectVersionID: versionID, ObservedSeq: 1, Generation: 2, ChunkID: "chk_p2a",
		SpanRefs: []source.Span{{Ref: source.ObjectRef{TenantID: tenant, ObjectID: objectID, ObservedSeq: 1}, BlockID: "blk_p1", ByteStart: 0, ByteEnd: 5}},
	}); err != nil {
		t.Fatalf("save generation-2 evidence: %v", err)
	}
	if _, err := svc.ReadEvidence(ctx, p, "ev_proj2"); err != nil {
		t.Fatalf("read generation-2 evidence: %v", err)
	}

	// A durable failed embed job projects the real error on the document row.
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO jobs (tenant_id, id, operation, status, idempotency_key, input_hash,
		                  input_ref, principal, submitter_key, policy, error)
		VALUES ($1, 'job_proj', 'rag.embed', 'failed', 'rag.embed:obj_proj:1:2', 'hash',
		        $2, '{}', 'tester', '{"generation":2}', '{"message":"boom: embedding failed"}')`,
		tenant, versionID); err != nil {
		t.Fatalf("insert failed job: %v", err)
	}
	doc, err = repo.GetDocument(ctx, tenant, objectID)
	if err != nil {
		t.Fatalf("get failed document: %v", err)
	}
	if doc.Status != rag.DocumentStatusFailed || doc.ErrorMessage != "boom: embedding failed" {
		t.Fatalf("failed projection mismatch: %#v", doc)
	}
	failed, err := repo.ListDocuments(ctx, tenant, "ragcol_proj", rag.DocumentStatusFailed, 10, 0)
	if err != nil || len(failed) != 1 || failed[0].ErrorMessage != "boom: embedding failed" {
		t.Fatalf("failed filter mismatch: %v %#v", err, failed)
	}
	if ready, err := repo.ListDocuments(ctx, tenant, "ragcol_proj", rag.DocumentStatusReady, 10, 0); err != nil || len(ready) != 0 {
		t.Fatalf("ready filter must exclude failed rows: %v %#v", err, ready)
	}
	rawFailed, err := repo.ListFailedDocuments(ctx, tenant, 10, 0)
	if err != nil || len(rawFailed) != 1 || rawFailed[0].ID != objectID {
		t.Fatalf("raw failed documents: %v %#v", err, rawFailed)
	}
	if n, err := repo.CountDocuments(ctx, tenant, true); err != nil || n != 1 {
		t.Fatalf("count documents: %d, %v", n, err)
	}

	// Soft tombstone: management metadata only, excluded from member lists
	// and raw failure reads, counted only when removal is not excluded.
	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE collection_items SET removed_at = now() WHERE tenant_id = $1 AND collection_id = 'ragcol_proj' AND object_id = $2`,
		tenant, objectID); err != nil {
		t.Fatalf("mark removed: %v", err)
	}
	if _, err := repo.GetDocument(ctx, tenant, objectID); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("removed document must read as absent, got %v", err)
	}
	if all, err := repo.ListDocuments(ctx, tenant, "ragcol_proj", rag.DocumentStatusAll, 10, 0); err != nil || len(all) != 0 {
		t.Fatalf("all filter must exclude removed rows: %v %#v", err, all)
	}
	removed, err := repo.ListDocuments(ctx, tenant, "ragcol_proj", rag.DocumentStatusRemoved, 10, 0)
	if err != nil || len(removed) != 1 {
		t.Fatalf("removed filter: %v %#v", err, removed)
	}
	if removed[0].Status != rag.DocumentStatusRemoved || removed[0].TextHash != "" ||
		removed[0].QdrantPointCount != 0 || len(removed[0].Metadata) != 0 {
		t.Fatalf("removed projection must be metadata-only: %#v", removed[0])
	}
	if rawFailed, err := repo.ListFailedDocuments(ctx, tenant, 10, 0); err != nil || len(rawFailed) != 0 {
		t.Fatalf("removed rows must not surface as failures: %v %#v", err, rawFailed)
	}
	if n, err := repo.CountDocuments(ctx, tenant, true); err != nil || n != 0 {
		t.Fatalf("count excluding removed: %d, %v", n, err)
	}
	if n, err := repo.CountDocuments(ctx, tenant, false); err != nil || n != 1 {
		t.Fatalf("count including removed: %d, %v", n, err)
	}

	// Per-collection generation scoping: a staging generation in one
	// collection cannot mask another collection's published state.
	if err := repo.InsertCollection(ctx, rag.Collection{
		TenantID: tenant, ID: "ragcol_other", Name: "D", OwnerActorID: "user_1",
		QdrantCollection: "rag_ragcol_other", EmbeddingModel: "m",
		ChunkSize: 100, ChunkOverlap: 10, IsActive: true,
	}); err != nil {
		t.Fatalf("insert second collection: %v", err)
	}
	if err := repo.AddCollectionItem(ctx, tenant, "ragcol_other", objectID, "user_1"); err != nil {
		t.Fatalf("add item to second collection: %v", err)
	}
	stageChunks(t, repo, tenant, "ragcol_other", objectID, versionID, 2, "chk_o1")
	projDocs, err := repo.ListDocuments(ctx, tenant, "ragcol_proj", rag.DocumentStatusRemoved, 10, 0)
	if err != nil || len(projDocs) != 1 {
		t.Fatalf("first collection removed view: %v %#v", err, projDocs)
	}
	otherDocs, err := repo.ListDocuments(ctx, tenant, "ragcol_other", rag.DocumentStatusAll, 10, 0)
	if err != nil || len(otherDocs) != 1 {
		t.Fatalf("second collection list: %v %#v", err, otherDocs)
	}
	// The version-scoped embed failure is shared by every collection holding
	// the version, while the projected chunk count stays its own latest
	// generation's (1 staged chunk here, not the first collection's 2).
	if otherDocs[0].Status != rag.DocumentStatusFailed ||
		otherDocs[0].ErrorMessage != "boom: embedding failed" || otherDocs[0].QdrantPointCount != 1 {
		t.Fatalf("second collection projection mismatch: %#v", otherDocs[0])
	}
}
