package rag_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/jobs"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

// metaLiveFixture is the retrieval fixture wired to a REAL Qdrant, so the
// metadata filters are evaluated by the actual payload filter semantics
// (keyword match on an array, integer range), not a test double.
type metaLiveFixture struct {
	svc       *rag.Service
	repo      *postgres.RAGRepo
	vector    *rag.QdrantClient
	tenant    string
	principal auth.Principal
}

// TestSearchMetadataFiltersIntegration exercises container/author/date
// filters end to end: lexical search over a real Qdrant, filters ANDed
// with the access prefilter, link authors matching, and points indexed
// before the fields existed excluded by every filter.
func TestSearchMetadataFiltersIntegration(t *testing.T) {
	qdrantURL := strings.TrimSpace(os.Getenv("QDRANT_INTEGRATION_URL"))
	if qdrantURL == "" {
		t.Skip("QDRANT_INTEGRATION_URL is not set")
	}
	f := newMetaLiveFixture(t, qdrantURL)
	ctx := context.Background()
	collection, err := f.svc.CreateCollection(ctx, f.principal, rag.CreateCollectionInput{
		Name: "Metadata live " + metaSafeName(), ChunkSize: 200, ChunkOverlap: 20,
		ACL: map[string]any{"public": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.vector.DeleteCollection(context.Background(), collection.QdrantCollection) }()

	// alice threads: document authors plus the chunk link author dave.
	f.stageMetaDoc(t, collection, "obj_meta_eng", "metadata integration barnacle engine", `{"container":"#eng","authors":["alice","carol"],"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-02-01T00:00:00Z"}`,
		map[string]any{
			"container": "#eng", "authors": []any{"alice", "carol", "dave"}, "author": "alice",
			"source_created_at": "2026-01-01T00:00:00Z", "source_updated_at": "2026-02-01T00:00:00Z",
			"source_updated_ts": time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC).Unix(),
			"links":             []any{map[string]any{"offset": 0, "url": "https://fixture.invalid/m1", "author": "dave", "timestamp": "2026-02-01T00:00:00Z"}},
		}, nil)
	// bob issue: no link authors, later updated_at.
	f.stageMetaDoc(t, collection, "obj_meta_legal", "metadata integration cuttlefish legal", `{"container":"acme/legal","authors":["bob"],"updated_at":"2026-03-01T00:00:00Z"}`,
		map[string]any{
			"container": "acme/legal", "authors": []any{"bob"}, "author": "bob",
			"source_updated_at": "2026-03-01T00:00:00Z",
			"source_updated_ts": time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).Unix(),
		}, nil)
	// A point indexed before this change: no metadata payload at all.
	f.stageMetaDoc(t, collection, "obj_meta_legacy", "metadata integration legacy drift", `{"kind":"upload"}`, nil, nil)
	// Readable by the post-check (full rows exist, public collection) but
	// carrying an acl token the caller lacks: ONLY the access prefilter
	// keeps it out, so this point proves a metadata filter can never widen
	// results — if the filter replaced the access clauses, it would return.
	f.stageMetaDoc(t, collection, "obj_meta_secret", "metadata integration seahorse secret", `{"container":"#eng","authors":["alice"],"updated_at":"2026-02-01T00:00:00Z"}`,
		map[string]any{
			"container": "#eng", "authors": []any{"alice"}, "author": "alice",
			"source_updated_at": "2026-02-01T00:00:00Z",
			"source_updated_ts": time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC).Unix(),
		}, []string{"acl-secret-token"})

	search := func(t *testing.T, input rag.SearchInput) map[string]struct{} {
		t.Helper()
		input.CollectionID, input.Mode, input.Limit, input.Query = collection.ID, rag.QueryLexical, 10, "metadata integration"
		results, err := f.svc.Search(ctx, f.principal, input)
		if err != nil {
			t.Fatalf("search %+v: %v", input, err)
		}
		ids := map[string]struct{}{}
		for _, r := range results {
			ids[r.DocumentID] = struct{}{}
		}
		return ids
	}
	expect := func(t *testing.T, got map[string]struct{}, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for _, id := range want {
			if _, ok := got[id]; !ok {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}

	// Unfiltered: everything except the acl-denied point.
	expect(t, search(t, rag.SearchInput{}), "obj_meta_eng", "obj_meta_legal", "obj_meta_legacy")
	// Container exact match.
	expect(t, search(t, rag.SearchInput{Container: "#eng"}), "obj_meta_eng")
	expect(t, search(t, rag.SearchInput{Container: "acme/legal"}), "obj_meta_legal")
	// Author: document authors AND the chunk's link author.
	expect(t, search(t, rag.SearchInput{Author: "dave"}), "obj_meta_eng")
	expect(t, search(t, rag.SearchInput{Author: "carol"}), "obj_meta_eng")
	expect(t, search(t, rag.SearchInput{Author: "bob"}), "obj_meta_legal")
	expect(t, search(t, rag.SearchInput{Author: "nobody"}))
	// Date range over source_updated_ts (after inclusive, before exclusive).
	expect(t, search(t, rag.SearchInput{UpdatedAfter: "2026-02-15T00:00:00Z"}), "obj_meta_legal")
	expect(t, search(t, rag.SearchInput{UpdatedAfter: "2026-01-01T00:00:00Z"}), "obj_meta_eng", "obj_meta_legal")
	expect(t, search(t, rag.SearchInput{UpdatedBefore: "2026-02-15T00:00:00Z"}), "obj_meta_eng")
	expect(t, search(t, rag.SearchInput{UpdatedBefore: "2026-02-01T00:00:00Z"}))
	// Filters AND within themselves and with access: the secret point
	// matches this filter exactly yet must never appear.
	expect(t, search(t, rag.SearchInput{Container: "#eng", Author: "bob"}))
	expect(t, search(t, rag.SearchInput{Container: "#eng", UpdatedAfter: "2026-02-15T00:00:00Z"}))
	if _, err := f.svc.Search(ctx, f.principal, rag.SearchInput{CollectionID: collection.ID, Query: "metadata integration", UpdatedAfter: "nope"}); !errors.Is(err, rag.ErrInvalidSearchFilter) {
		t.Fatalf("malformed date bound = %v, want ErrInvalidSearchFilter", err)
	}

	// Semantic (dense) mode carries the same combined filter.
	results, err := f.svc.Search(ctx, f.principal, rag.SearchInput{CollectionID: collection.ID, Query: "metadata integration", Mode: rag.QuerySemantic, Limit: 10, Container: "acme/legal"})
	if err != nil {
		t.Fatalf("semantic search: %v", err)
	}
	if len(results) != 1 || results[0].DocumentID != "obj_meta_legal" {
		t.Fatalf("semantic + container filter = %#v", results)
	}
}

// The fake embedding model produces 3-dimension vectors (see
// fakeEmbeddingUpstream); the semantic subtest needs matching dense.
func newMetaLiveFixture(t *testing.T, qdrantURL string) *metaLiveFixture {
	t.Helper()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "rag-meta-live")
	user := testdb.SeedUser(t, db, tenant, "meta-live@example.test", nil)
	jobsSvc := jobs.NewService(postgres.NewJobsRepo(db), 0)
	store, err := artifacts.NewStore(t.TempDir(), t.TempDir(), postgres.NewArtifactRepo(db))
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	repo := postgres.NewRAGRepo(db)
	executor := newExecutor(t, db, tenant, &fakeEmbeddingUpstream{})
	vector := rag.NewQdrantClient(qdrantURL, 15*time.Second)
	svc := rag.NewService(repo, vector, executor, "embed-test", store, jobsSvc)
	svc.SetIndexRetirementSoak(time.Nanosecond)
	return &metaLiveFixture{
		svc: svc, repo: repo, vector: vector, tenant: tenant,
		principal: auth.Principal{
			TenantID: tenant, ActorID: user.ID, ActorKind: auth.ActorKindUser, UserID: user.ID,
			CredentialID: "meta-live-key", CredentialKind: auth.CredentialKindAPIKey,
			Scopes:              map[string]struct{}{"rag": {}, "rag.manage": {}},
			AllowedModelsAll:    true,
			AllowedEndpointsAll: true,
		},
	}
}

// stageMetaDoc mirrors stageSearchDoc and adds the v1.2 metadata payload
// fields; acl (optional) simulates a projected point whose tokens the
// caller lacks.
func (f *metaLiveFixture) stageMetaDoc(t *testing.T, collection rag.Collection, objectID, content, provenance string, extra map[string]any, acl []string) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.repo.UpsertObject(ctx, source.Object{
		TenantID: f.tenant, ID: objectID, Kind: "document", NativeID: "native-" + objectID,
		AuthorizationMode: source.AuthorizationCollection, OwnerActorID: f.principal.ActorID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.InsertObjectVersion(ctx, f.tenant, source.ObjectVersion{
		ObjectID: objectID, ObservedSeq: 1, ArtifactRef: "art_" + objectID, SHA256: "sha_" + objectID,
		ByteSize: int64(len(content)), MimeType: "text/plain", OriginalFilename: objectID + ".txt",
		Provenance: provenance,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.AddCollectionItem(ctx, f.tenant, collection.ID, objectID, f.principal.ActorID); err != nil {
		t.Fatal(err)
	}
	parsed := source.Document{
		Ref: source.ObjectRef{TenantID: f.tenant, ObjectID: objectID, ObservedSeq: 1},
		Blocks: []source.Block{
			{ID: "blk_" + objectID, Ordinal: 0, Kind: "paragraph", Text: content, ByteStart: 0, ByteEnd: len(content)},
		},
	}
	if err := f.repo.SaveParsedDocument(ctx, "rag-structured-v1", parsed); err != nil {
		t.Fatal(err)
	}
	profileID, err := f.repo.EnsureChunkProfile(ctx, f.tenant, "rag-"+collection.ID+"-structured", 200, 20, rag.ChunkStrategyFixed)
	if err != nil {
		t.Fatal(err)
	}
	chunkID := "chk_" + objectID
	chunks := []rag.DocumentChunk{{
		ID: chunkID, DocumentID: objectID, CollectionID: collection.ID, Generation: 1,
		ChunkIndex: 0, Content: content, TokenCount: 4,
		SpanRefs: []source.Span{{Ref: parsed.Ref, BlockID: "blk_" + objectID, ByteStart: 0, ByteEnd: 5}},
	}}
	if err := f.repo.ReplaceVersionChunksAndStage(ctx, f.tenant, collection.ID, objectID, objectID+"#1", 1, 1, profileID, chunks); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.MarkGenerationState(ctx, f.tenant, collection.ID, objectID+"#1", 1, "published"); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"tenant_id": f.tenant, "document_id": objectID, "object_version_id": objectID + "#1",
		"generation": int64(1), "chunk_id": chunkID, "file_name": objectID + ".txt",
		"content": content, "access_enabled": true,
	}
	for key, value := range extra {
		payload[key] = value
	}
	if len(acl) > 0 {
		payload["acl"] = acl
	}
	// The fake embedding model produces 3-dimension vectors (see
	// fakeEmbeddingUpstream); the semantic subtest needs matching dense.
	if err := f.vector.EnsureCollection(ctx, collection.QdrantCollection, 3); err != nil {
		t.Fatalf("ensure collection: %v", err)
	}
	if err := f.vector.UpsertPoints(ctx, collection.QdrantCollection, []rag.QdrantPoint{{
		ID: ragPointID(), Vector: []float64{0.1, 0.2, 0.3}, Payload: payload,
	}}); err != nil {
		t.Fatalf("upsert %s points: %v", objectID, err)
	}
}

func metaSafeName() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }
