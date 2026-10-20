package rag_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

// envOrFatal requires an explicitly configured integration prerequisite —
// a missing setup fails, never skips.
func envOrFatal(t *testing.T, name string) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		t.Fatalf("%s is not set; this test requires the configured prerequisite", name)
	}
	return v
}

// TestQdrantClientIntegration exercises the live vector store directly.
func TestQdrantClientIntegration(t *testing.T) {
	qdrantURL := envOrFatal(t, "QDRANT_INTEGRATION_URL")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client := rag.NewQdrantClient(qdrantURL, 5*time.Second)
	collection := "rag_integration_" + ragSafeName()
	defer func() { _ = client.DeleteCollection(context.Background(), collection) }()
	if err := client.EnsureCollection(ctx, collection, 3); err != nil {
		t.Fatalf("ensure collection: %v", err)
	}
	if err := client.UpsertPoints(ctx, collection, []rag.QdrantPoint{{
		ID:     ragPointID(),
		Vector: []float64{0.1, 0.2, 0.3},
		Sparse: integrationSparse("qdrant integration content"),
		Payload: map[string]any{
			"document_id": "doc_qdrant_integration",
			"chunk_id":    "chunk_qdrant_integration",
			"content":     "qdrant integration content",
		},
	}}); err != nil {
		t.Fatalf("upsert points: %v", err)
	}
	results, err := client.Search(ctx, collection, []float64{0.1, 0.2, 0.3}, 1, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 || results[0].Payload["document_id"] != "doc_qdrant_integration" {
		t.Fatalf("unexpected search results: %#v", results)
	}
	if err := client.DeleteDocumentPoints(ctx, collection, "doc_qdrant_integration"); err != nil {
		t.Fatalf("delete document points: %v", err)
	}
	results, err = client.Search(ctx, collection, []float64{0.1, 0.2, 0.3}, 1, nil)
	if err != nil {
		t.Fatalf("search after delete: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected deleted document points to disappear, got %#v", results)
	}
}

func TestQdrantHybridAndLegacyIntegration(t *testing.T) {
	baseURL := envOrFatal(t, "QDRANT_INTEGRATION_URL")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := rag.NewQdrantClient(baseURL, 5*time.Second)
	collection := "rag_hybrid_" + ragSafeName()
	defer func() { _ = client.DeleteCollection(context.Background(), collection) }()
	if err := client.EnsureCollection(ctx, collection, 2); err != nil {
		t.Fatalf("ensure hybrid collection: %v", err)
	}
	targetID, otherID := ragPointID(), ragPointID()
	if err := client.UpsertPoints(ctx, collection, []rag.QdrantPoint{
		{ID: targetID, Vector: []float64{1, 0}, Sparse: integrationSparse("orchid meadow"), Payload: map[string]any{"document_id": "orchid", "tenant_id": "tenant-a"}},
		{ID: otherID, Vector: []float64{0, 1}, Sparse: integrationSparse("quartz harbor"), Payload: map[string]any{"document_id": "quartz", "tenant_id": "tenant-b"}},
	}); err != nil {
		t.Fatalf("upsert hybrid points: %v", err)
	}
	filter := map[string]any{"must": []any{map[string]any{"key": "tenant_id", "match": map[string]any{"value": "tenant-a"}}}}
	for _, tc := range []struct {
		name   string
		vector []float64
		sparse rag.SparseVector
	}{
		{"sparse-only terms", []float64{0, 1}, integrationSparse("orchid meadow")},
		{"dense-only terms", []float64{1, 0}, integrationSparse("unrelated")},
		{"both", []float64{1, 0}, integrationSparse("orchid meadow")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			results, err := client.SearchHybrid(ctx, collection, tc.vector, tc.sparse, 2, filter)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 || results[0].ID != targetID {
				t.Fatalf("expected authorized orchid point, got %#v", results)
			}
		})
	}
	legacy := "rag_legacy_" + ragSafeName()
	defer func() { _ = client.DeleteCollection(context.Background(), legacy) }()
	create, _ := json.Marshal(map[string]any{"vectors": map[string]any{"size": 2, "distance": "Cosine"}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, baseURL+"/collections/"+legacy, bytes.NewReader(create))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("create legacy collection: %s", resp.Status)
	}
	legacyID := ragPointID()
	if err := client.UpsertPoints(ctx, legacy, []rag.QdrantPoint{{ID: legacyID, Vector: []float64{1, 0}, Payload: map[string]any{"document_id": "legacy"}}}); err != nil {
		t.Fatalf("upsert legacy point: %v", err)
	}
	results, err := client.SearchHybrid(ctx, legacy, []float64{1, 0}, integrationSparse("legacy words"), 1, nil)
	if err != nil {
		t.Fatalf("search legacy collection: %v", err)
	}
	if len(results) != 1 || results[0].ID != legacyID {
		t.Fatalf("unexpected legacy search results: %#v", results)
	}
}

func integrationSparse(text string) rag.SparseVector {
	indices := make([]uint32, 0)
	for _, token := range strings.Fields(text) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(token))
		indices = append(indices, h.Sum32())
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
	values := make([]float32, len(indices))
	for i := range values {
		values[i] = 1
	}
	return rag.SparseVector{Indices: indices, Values: values}
}

// TestEmbeddingExecutorIntegration indexes a small text fixture end-to-end:
// real PostgreSQL, real Qdrant and a real OpenAI-compatible embedding
// endpoint reached through the shared priced executor — one dispatched
// reservation per bounded batch, settled against the immutable rate.
func TestEmbeddingExecutorIntegration(t *testing.T) {
	qdrantURL := envOrFatal(t, "QDRANT_INTEGRATION_URL")
	baseOrigin := strings.TrimSpace(os.Getenv("OPENAI_CONTRACT_BASE_URL"))
	if baseOrigin == "" || strings.TrimSpace(os.Getenv("OPENAI_CONTRACT_API_KEY")) == "" {
		t.Skip("OPENAI_CONTRACT_BASE_URL/OPENAI_CONTRACT_API_KEY not set; live embedding upstream not requested")
	}
	model := strings.TrimSpace(os.Getenv("ENCLAVA_TEST_EMBEDDING_MODEL"))
	if model == "" {
		model = "text-embedding-3-small"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "rag-embed-live")
	user := testdb.SeedUser(t, db, tenant, "rag-embed-live@example.test", nil)
	p := auth.Principal{
		TenantID: tenant, ActorID: user.ID, ActorKind: auth.ActorKindUser, UserID: user.ID,
		CredentialID: "key_test", CredentialKind: auth.CredentialKindAPIKey,
		AllowedModelsAll: true, AllowedEndpointsAll: true,
	}

	origin, err := auth.CanonicalizeSecretOrigin(baseOrigin)
	if err != nil {
		t.Fatalf("canonicalize origin: %v", err)
	}
	resolver, err := auth.NewSecretResolver([]auth.SecretBinding{{
		RefID: "embed-contract-key", TenantID: tenant,
		Purpose: auth.SecretPurposeInference, EnvName: "OPENAI_CONTRACT_API_KEY",
		Origins: []string{origin},
	}})
	if err != nil {
		t.Fatalf("secret resolver: %v", err)
	}
	endpoints := postgres.NewEndpointsRepo(db)
	if err := endpoints.Create(ctx, inference.Endpoint{
		ID: "ep-embed-live", TenantID: tenant, Protocol: "openai_compatible",
		BaseOrigin:        origin,
		CredentialRef:     "embed-contract-key",
		Capabilities:      []string{inference.CapabilityEmbeddings, "model:" + model},
		AuthorizedPrivate: true, // the contract fixture may point at a local upstream
		Enabled:           true,
	}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	billingRepo := postgres.NewBillingRepo(db)
	if err := billingRepo.PutPrice(ctx, tenant, "p-embed-live", billing.Price{
		SubjectType: billing.PriceSubjectEndpoint, SubjectID: "ep-embed-live",
		Capability: inference.CapabilityEmbeddings, Unit: billing.UnitInput,
		RateMicros: 1, UnitQuantity: 1, Currency: "USD",
	}, "tester", time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("put price: %v", err)
	}
	if err := billingRepo.PutPrice(ctx, tenant, "p-embed-live-cached", billing.Price{
		SubjectType: billing.PriceSubjectEndpoint, SubjectID: "ep-embed-live",
		Capability: inference.CapabilityEmbeddings, Unit: billing.UnitCachedInput,
		RateMicros: 1, UnitQuantity: 1, Currency: "USD",
	}, "tester", time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("put price: %v", err)
	}
	sources := postgres.NewSourceRepo(db)
	executor, err := inference.NewService(inference.Deps{
		Endpoints:  endpoints,
		Sources:    sources,
		Authorizer: source.NewAuthorizer(sources, 0),
		Billing:    billing.NewService(billingRepo),
		Secrets:    resolver,
		Artifacts:  postgres.NewArtifactRepo(db),
	})
	if err != nil {
		t.Fatalf("new inference service: %v", err)
	}

	jobsRepo := postgres.NewJobsRepo(db)
	jobsSvc := jobs.NewService(jobsRepo, 0)
	artifactStore, err := artifacts.NewStore(t.TempDir(), t.TempDir(), postgres.NewArtifactRepo(db))
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	vector := rag.NewQdrantClient(qdrantURL, 15*time.Second)
	repo := postgres.NewRAGRepo(db)
	svc := rag.NewService(repo, vector, executor, model, artifactStore, jobsSvc)

	c, err := svc.CreateCollection(ctx, p, rag.CreateCollectionInput{
		Name: "Live " + ragSafeName(), EmbeddingModel: model,
		ChunkSize: 500, ChunkOverlap: 50, ACL: map[string]any{"public": true},
	})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	defer func() { _ = vector.DeleteCollection(context.Background(), c.QdrantCollection) }()

	upload, err := svc.UploadDocument(ctx, p, c.ID, rag.UploadDocumentInput{
		FileName: "live.txt", MimeType: "text/plain",
		Bytes: []byte("enclava live embedding integration fixture content"),
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	claim, err := jobsSvc.Lease(ctx, "live-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("lease embed job: claim=%v err=%v", claim, err)
	}
	resultRef, err := svc.JobHandler()(ctx, claim, nil)
	if err != nil {
		_ = claim.Fail(ctx, err, jobs.FailureFatal)
		t.Fatalf("embed attempt: %v", err)
	}
	if _, err := claim.Complete(ctx, resultRef); err != nil {
		t.Fatalf("complete: %v", err)
	}

	results, err := svc.Search(ctx, p, rag.SearchInput{CollectionID: c.ID, Query: "live embedding", Limit: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 || results[0].DocumentID != upload.Document.ID {
		t.Fatalf("unexpected live results: %#v", results)
	}
	markdown, err := svc.UploadDocument(ctx, p, c.ID, rag.UploadDocumentInput{
		FileName: "sections.md", MimeType: "text/markdown",
		Bytes: []byte("# Doc\n\n## Section\n\nsectioncite unique marker"),
	})
	if err != nil {
		t.Fatalf("upload markdown: %v", err)
	}
	claim, err = jobsSvc.Lease(ctx, "live-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("lease markdown embed job: claim=%v err=%v", claim, err)
	}
	resultRef, err = svc.JobHandler()(ctx, claim, nil)
	if err != nil {
		_ = claim.Fail(ctx, err, jobs.FailureFatal)
		t.Fatalf("markdown embed attempt: %v", err)
	}
	if _, err := claim.Complete(ctx, resultRef); err != nil {
		t.Fatalf("complete markdown embed job: %v", err)
	}
	citations, err := svc.Search(ctx, p, rag.SearchInput{
		CollectionID: c.ID, Query: "sectioncite unique marker", Limit: 5,
	})
	if err != nil {
		t.Fatalf("search markdown section: %v", err)
	}
	if len(citations) == 0 || citations[0].DocumentID != markdown.Document.ID || citations[0].HeaderPath != "Doc › Section" {
		t.Fatalf("markdown search citation lacks section path: %#v", citations)
	}
}

func ragPointID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:])
}

func ragSafeName() string {
	n := strings.NewReplacer("-", "", "_", "").Replace(time.Now().UTC().Format("20060102150405.000000000"))
	return "t" + strings.ReplaceAll(n, ".", "")
}
