package rag_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

// TestIngestSelectedProfileReachesRealIndexer is the B5 selected-profile
// regression (mapper prerequisite) at the real-indexer level: the
// collection's selected immutable chunk_profile_version_id must drive the
// ingest worker's chunking AND the points actually upserted into Qdrant —
// never a silently-defaulted profile. Requires QDRANT_INTEGRATION_URL; a
// chunk-only substitute is NOT this regression.
func TestIngestSelectedProfileReachesRealIndexer(t *testing.T) {
	qdrantURL := envOrFatal(t, "QDRANT_INTEGRATION_URL")
	ctx := context.Background()

	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "rag-ingest-profile")
	user := testdb.SeedUser(t, db, tenant, "ingest-owner@example.test", nil)
	jobsSvc := jobs.NewService(postgres.NewJobsRepo(db), 0)
	artifactStore, err := artifacts.NewStore(t.TempDir(), t.TempDir(), postgres.NewArtifactRepo(db))
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	repo := postgres.NewRAGRepo(db)
	vector := rag.NewQdrantClient(qdrantURL, 15*time.Second)
	embedding := &fakeEmbeddingUpstream{}
	svc := rag.NewService(repo, vector, newExecutor(t, db, tenant, embedding), "embed-test", artifactStore, jobsSvc)
	p := auth.Principal{
		TenantID: tenant, ActorID: user.ID, UserID: user.ID,
		ActorKind: auth.ActorKindUser, CredentialID: "key_test", CredentialKind: auth.CredentialKindAPIKey,
		Scopes:              map[string]struct{}{"rag": {}, "rag.manage": {}},
		AllowedModelsAll:    true,
		AllowedEndpointsAll: true,
	}

	// Non-default profile: tiny chunk size, fixed strategy — nothing any
	// fallback default could produce.
	profiles := postgres.NewChunkProfilesRepo(db)
	profile, err := profiles.Create(ctx, tenant, source.ChunkProfile{
		ProfileID: "ingest-selected", SizeCodePoints: 60, OverlapCodePoints: 8,
		BoundaryStrategy: "fixed", TableHandling: "row-wise",
	}, p.ActorID)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	collection, err := svc.CreateCollection(ctx, p, rag.CreateCollectionInput{
		Name: "profiled", ChunkProfileVersionID: &profile.ID,
		ACL: map[string]any{"public": true},
	})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	if collection.ChunkProfileVersionID != profile.ID {
		t.Fatalf("collection bound to %q, want selected %q", collection.ChunkProfileVersionID, profile.ID)
	}
	defer func() { _ = vector.DeleteCollection(context.Background(), collection.QdrantCollection) }()

	content := strings.Repeat("indexer profile probe sentence. ", 10) // ~320 code points
	upload, err := svc.UploadDocument(ctx, p, collection.ID, rag.UploadDocumentInput{
		FileName: "probe.txt", MimeType: "text/plain", Bytes: []byte(content),
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	// The real worker path: lease → handler → fenced complete.
	claim, err := jobsSvc.Lease(ctx, "profile-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("lease: claim=%v err=%v", claim, err)
	}
	if got := claim.Job().Policy["chunk_profile_version_id"]; got != profile.ID {
		t.Fatalf("queued policy profile = %#v, want the selected %q", got, profile.ID)
	}
	resultRef, err := svc.JobHandler()(ctx, claim, nil)
	if err != nil {
		t.Fatalf("ingest worker: %v", err)
	}
	if _, err := claim.Complete(ctx, resultRef); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// The staged generation carries the SELECTED immutable profile id.
	var generationProfile string
	if err := db.SQL.QueryRowContext(ctx, `
		SELECT g.chunk_profile_version_id FROM index_generations g
		WHERE g.tenant_id=$1 AND g.collection_id=$2 AND g.object_version_id=$3`,
		tenant, collection.ID, upload.Document.ID+"#1").Scan(&generationProfile); err != nil {
		t.Fatalf("read generation: %v", err)
	}
	if generationProfile != profile.ID {
		t.Fatalf("generation bound to %q, want selected %q", generationProfile, profile.ID)
	}

	// Durable chunks reflect the selected 60-code-point profile — a
	// defaulted ~1000-code-point profile would produce exactly one chunk.
	chunks, err := repo.ListGenerationChunks(ctx, tenant, collection.ID, upload.Document.ID+"#1", 1)
	if err != nil {
		t.Fatalf("list chunks: %v", err)
	}
	if len(chunks) < 3 {
		t.Fatalf("chunks=%d, want the selected 60-code-point chunking of ~320 code points", len(chunks))
	}
	for _, chunk := range chunks {
		if n := len([]rune(chunk.Content)); n > 60 {
			t.Fatalf("chunk %d exceeds selected profile size: %d code points", chunk.ChunkIndex, n)
		}
		if chunk.DocumentID != upload.Document.ID {
			t.Fatalf("chunk DocumentID = %q, want object id %q", chunk.DocumentID, upload.Document.ID)
		}
	}

	// The real index consumed the same output: exactly len(chunks) points in
	// this generation's Qdrant collection, each carrying the version id.
	var pointCount int
	scroll, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/collections/%s/points/count", qdrantURL, collection.QdrantCollection),
		strings.NewReader(`{"exact":true}`))
	if err != nil {
		t.Fatal(err)
	}
	scroll.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(scroll)
	if err != nil {
		t.Fatalf("qdrant count: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var countPayload struct {
		Result struct {
			Count int `json:"count"`
		} `json:"result"`
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("qdrant count status = %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&countPayload); err != nil {
		t.Fatalf("decode count: %v", err)
	}
	pointCount = countPayload.Result.Count
	if pointCount != len(chunks) {
		t.Fatalf("indexed points = %d, want the selected profile's %d chunks", pointCount, len(chunks))
	}
}
