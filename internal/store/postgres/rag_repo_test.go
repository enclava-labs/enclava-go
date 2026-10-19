package postgres_test

import (
	"context"
	"errors"
	"testing"

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

func TestRAGRepoDocumentProjectionAndGenerations(t *testing.T) {
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
	gen, err := repo.NextGeneration(ctx, tenant, "ragcol_1", doc.ObjectVersionID)
	if err != nil || gen != 1 {
		t.Fatalf("next generation = %d, %v", gen, err)
	}
	chunks := []rag.DocumentChunk{{
		ID: "chk_1", DocumentID: obj.ID, CollectionID: "ragcol_1",
		ChunkIndex: 0, Content: "hello", TokenCount: 1,
	}}
	if err := repo.ReplaceVersionChunksAndStage(ctx, tenant, "ragcol_1", obj.ID, doc.ObjectVersionID, 1, gen, "", chunks); err == nil {
		t.Fatal("expected chunk insert to fail without a bound profile")
	}
	profileID, err := repo.EnsureChunkProfile(ctx, tenant, "rag-ragcol_1", 100, 10)
	if err != nil {
		t.Fatalf("ensure profile: %v", err)
	}
	if err := repo.ReplaceVersionChunksAndStage(ctx, tenant, "ragcol_1", obj.ID, doc.ObjectVersionID, 1, gen, profileID, chunks); err != nil {
		t.Fatalf("stage chunks+generation: %v", err)
	}
	if n, err := repo.ChunkCountForVersion(ctx, tenant, doc.ObjectVersionID); err != nil || n != 1 {
		t.Fatalf("chunk count = %d, %v", n, err)
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
	listed, err := repo.ListDocuments(ctx, tenant, "ragcol_1", 10, 0)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list documents: %v %#v", err, listed)
	}
}
