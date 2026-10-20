package rag_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// fakeEmbeddingUpstream is the deterministic executor transport answering
// embeddings requests — real policy/pricing run inside inference.Service.
type fakeEmbeddingUpstream struct {
	calls int
}

func (f *fakeEmbeddingUpstream) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	return nil, errors.New("unexpected chat call")
}

func (f *fakeEmbeddingUpstream) CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	return nil, errors.New("unexpected chat stream call")
}

func (f *fakeEmbeddingUpstream) DoResponsesRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	return nil, errors.New("unexpected responses call")
}

func (f *fakeEmbeddingUpstream) DoResponsesStream(ctx context.Context, method, path string, body []byte) (io.ReadCloser, error) {
	return nil, errors.New("unexpected responses stream call")
}

func (f *fakeEmbeddingUpstream) CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error) {
	f.calls++
	var payload struct {
		Input any `json:"input"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	inputs := []string{}
	switch v := payload.Input.(type) {
	case string:
		inputs = append(inputs, v)
	case []any:
		for _, item := range v {
			s, _ := item.(string)
			inputs = append(inputs, s)
		}
	}
	data := make([]map[string]any, 0, len(inputs))
	for i, input := range inputs {
		data = append(data, map[string]any{
			"index":     i,
			"embedding": []float64{float64(len(input)), float64(strings.Count(input, "enclava") + 1), 1},
		})
	}
	return json.Marshal(map[string]any{
		"object": "list",
		"data":   data,
		"usage":  map[string]any{"prompt_tokens": len(inputs), "total_tokens": len(inputs)},
	})
}

// newExecutor composes the real shared executor over postgres seams with the
// fake transport, plus the embeddings endpoint binding and price the
// attempt must resolve through.
func newExecutor(t *testing.T, db *postgres.DB, tenant string, upstream inference.Upstream) *inference.Service {
	t.Helper()
	ctx := context.Background()
	endpoints := postgres.NewEndpointsRepo(db)
	sources := postgres.NewSourceRepo(db)
	billingRepo := postgres.NewBillingRepo(db)
	if err := endpoints.Create(ctx, inference.Endpoint{
		ID:           "ep-embed",
		TenantID:     tenant,
		Protocol:     "openai_compatible",
		BaseOrigin:   "https://embed.upstream.example",
		Capabilities: []string{inference.CapabilityEmbeddings, "model:embed-test"},
		Enabled:      true,
	}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	if err := billingRepo.PutPrice(ctx, tenant, fmt.Sprintf("p-embed-%s", newTestID()), billing.Price{
		SubjectType: billing.PriceSubjectEndpoint, SubjectID: "ep-embed",
		Capability: inference.CapabilityEmbeddings, Unit: billing.UnitInput,
		RateMicros: 1, UnitQuantity: 1, Currency: "USD",
	}, "tester", time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("put price: %v", err)
	}
	if err := billingRepo.PutPrice(ctx, tenant, fmt.Sprintf("p-embed-cached-%s", newTestID()), billing.Price{
		SubjectType: billing.PriceSubjectEndpoint, SubjectID: "ep-embed",
		Capability: inference.CapabilityEmbeddings, Unit: billing.UnitCachedInput,
		RateMicros: 1, UnitQuantity: 1, Currency: "USD",
	}, "tester", time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("put price: %v", err)
	}
	svc, err := inference.NewService(inference.Deps{
		Endpoints:  endpoints,
		Sources:    sources,
		Authorizer: source.NewAuthorizer(sources, 0),
		Billing:    billing.NewService(billingRepo),
		Artifacts:  postgres.NewArtifactRepo(db),
	}, inference.WithTransportFactory(func(ctx context.Context, e inference.Endpoint, credential string) (inference.Upstream, error) {
		return upstream, nil
	}))
	if err != nil {
		t.Fatalf("new inference service: %v", err)
	}
	return svc
}

func newTestID() string { return fmt.Sprintf("t%d", time.Now().UnixNano()) }

type fakeVectorStore struct {
	points map[string][]rag.QdrantPoint
}

func newFakeVectorStore() *fakeVectorStore {
	return &fakeVectorStore{points: map[string][]rag.QdrantPoint{}}
}

func (f *fakeVectorStore) EnsureCollection(ctx context.Context, collection string, vectorSize int) error {
	if _, ok := f.points[collection]; !ok {
		f.points[collection] = nil
	}
	return nil
}

func (f *fakeVectorStore) UpsertPoints(ctx context.Context, collection string, points []rag.QdrantPoint) error {
	f.points[collection] = append(f.points[collection], points...)
	return nil
}

func (f *fakeVectorStore) DeleteDocumentPoints(ctx context.Context, collection, documentID string) error {
	kept := []rag.QdrantPoint{}
	for _, point := range f.points[collection] {
		if point.Payload["document_id"] != documentID {
			kept = append(kept, point)
		}
	}
	f.points[collection] = kept
	return nil
}

func (f *fakeVectorStore) DeleteCollection(ctx context.Context, collection string) error {
	delete(f.points, collection)
	return nil
}

// Search honours the authorization-scoped filter the service passes —
// points for non-authorized version ids never match.
func (f *fakeVectorStore) SearchHybrid(ctx context.Context, collection string, vector []float64, sparse rag.SparseVector, limit int, filter map[string]any) ([]rag.QdrantSearchPoint, error) {
	allowed := map[string]bool{}
	if filter != nil {
		if must, ok := filter["must"].([]any); ok {
			for _, clause := range must {
				m, ok := clause.(map[string]any)
				if !ok {
					continue
				}
				if m["key"] == "object_version_id" {
					if match, ok := m["match"].(map[string]any); ok {
						if anyList, ok := match["any"].([]string); ok {
							for _, v := range anyList {
								allowed[v] = true
							}
						}
					}
				}
			}
		}
	}
	out := []rag.QdrantSearchPoint{}
	for _, point := range f.points[collection] {
		if len(allowed) > 0 && !allowed[point.Payload["object_version_id"].(string)] {
			continue
		}
		out = append(out, rag.QdrantSearchPoint{ID: point.ID, Score: 0.99, Payload: point.Payload})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

type fixture struct {
	db        *postgres.DB
	svc       *rag.Service
	repo      *postgres.RAGRepo
	jobs      *jobs.Service
	store     *artifacts.Store
	vector    *fakeVectorStore
	tenant    string
	principal auth.Principal
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "rag")
	user := testdb.SeedUser(t, db, tenant, "rag-owner@example.test", nil)
	jobsRepo := postgres.NewJobsRepo(db)
	jobsSvc := jobs.NewService(jobsRepo, 0)
	artifactRepo := postgres.NewArtifactRepo(db)
	store, err := artifacts.NewStore(t.TempDir(), t.TempDir(), artifactRepo)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	vector := newFakeVectorStore()
	repo := postgres.NewRAGRepo(db)
	svc := rag.NewService(repo, vector, newExecutor(t, db, tenant, &fakeEmbeddingUpstream{}), "embed-test", store, jobsSvc)
	p := auth.Principal{
		TenantID:            tenant,
		ActorID:             user.ID,
		ActorKind:           auth.ActorKindUser,
		UserID:              user.ID,
		CredentialID:        "key_test",
		CredentialKind:      auth.CredentialKindAPIKey,
		AllowedModelsAll:    true,
		AllowedEndpointsAll: true,
	}
	return &fixture{db: db, svc: svc, repo: repo, jobs: jobsSvc, store: store, vector: vector, tenant: tenant, principal: p}
}

func (f *fixture) collection(t *testing.T) rag.Collection {
	t.Helper()
	c, err := f.svc.CreateCollection(context.Background(), f.principal, rag.CreateCollectionInput{
		Name:         "Docs",
		ChunkSize:    100,
		ChunkOverlap: 10,
		ACL:          map[string]any{"public": true},
	})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	return c
}

// runNextJob leases the next queued job, invokes the registered embed
// handler and completes it — the real worker path under the fenced lease.
func (f *fixture) runNextJob(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	claim, err := f.jobs.Lease(ctx, "test-worker", time.Minute)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if claim == nil {
		t.Fatal("no job to lease")
	}
	resultRef, err := f.svc.JobHandler()(ctx, claim, nil)
	if err != nil {
		_ = claim.Fail(ctx, err, jobs.FailureFatal)
		t.Fatalf("embed attempt: %v", err)
	}
	if _, err := claim.Complete(ctx, resultRef); err != nil {
		t.Fatalf("complete: %v", err)
	}
}

func (f *fixture) docStatus(t *testing.T, collectionID, docID string) rag.Document {
	t.Helper()
	docs, err := f.svc.ListDocuments(context.Background(), f.principal, collectionID, 100, 0)
	if err != nil {
		t.Fatalf("list documents: %v", err)
	}
	for _, d := range docs {
		if d.ID == docID {
			return d
		}
	}
	t.Fatalf("document %s not listed", docID)
	return rag.Document{}
}

func TestServiceUploadIndexSearchDelete(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)

	upload, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "note.txt",
		MimeType: "text/plain",
		Bytes:    []byte("enclava rag search content for integration testing"),
	})
	if err != nil {
		t.Fatalf("upload document: %v", err)
	}
	if upload.Document.Status != rag.DocumentStatusProcessing {
		t.Fatalf("expected processing before the embed job runs, got %s", upload.Document.Status)
	}
	f.runNextJob(t)
	doc := f.docStatus(t, c.ID, upload.Document.ID)
	if doc.Status != rag.DocumentStatusReady {
		t.Fatalf("expected ready document after embed, got %s", doc.Status)
	}

	results, err := f.svc.Search(ctx, f.principal, rag.SearchInput{CollectionID: c.ID, Query: "enclava search", Limit: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 || results[0].DocumentID != upload.Document.ID {
		t.Fatalf("unexpected search results: %#v", results)
	}
	if _, blob, err := f.svc.DownloadDocument(ctx, f.principal, upload.Document.ID); err != nil || string(blob) == "" {
		t.Fatalf("download: %v", err)
	}
	if err := f.svc.DeleteDocument(ctx, f.principal, upload.Document.ID); err != nil {
		t.Fatalf("delete document: %v", err)
	}
	if got := len(f.vector.points[c.QdrantCollection]); got != 0 {
		t.Fatalf("expected qdrant points removed, got %d", got)
	}
}

func TestServiceTenantAndGrantDeny(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c, err := f.svc.CreateCollection(ctx, f.principal, rag.CreateCollectionInput{
		Name:         "Private",
		ChunkSize:    100,
		ChunkOverlap: 10,
		ACL:          map[string]any{"public": false},
	})
	if err != nil {
		t.Fatalf("create private collection: %v", err)
	}
	upload, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "secret.txt",
		MimeType: "text/plain",
		Bytes:    []byte("private doc content phrase"),
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	f.runNextJob(t)

	// A second tenant's principal must see nothing — deny, never leak.
	otherTenant := testdb.SeedTenant(t, f.db, "other")
	otherUser := testdb.SeedUser(t, f.db, otherTenant, "intruder@example.test", nil)
	intruder := auth.Principal{TenantID: otherTenant, ActorID: otherUser.ID, ActorKind: auth.ActorKindUser, UserID: otherUser.ID}
	if _, err := f.svc.GetCollection(ctx, intruder, c.ID); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("expected cross-tenant deny, got %v", err)
	}
	results, err := f.svc.Search(ctx, intruder, rag.SearchInput{CollectionID: c.ID, Query: "private", Limit: 5})
	if err == nil && len(results) != 0 {
		t.Fatalf("expected cross-tenant search to return nothing, got %#v", results)
	}
	if _, _, err := f.svc.DownloadDocument(ctx, intruder, upload.Document.ID); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("expected cross-tenant download deny, got %v", err)
	}

	// Same tenant, non-owner, non-audience: collection hidden, document denied.
	stranger := testdb.SeedUser(t, f.db, f.tenant, "stranger@example.test", nil)
	sp := auth.Principal{TenantID: f.tenant, ActorID: stranger.ID, ActorKind: auth.ActorKindUser, UserID: stranger.ID}
	if _, err := f.svc.GetCollection(ctx, sp, c.ID); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("expected same-tenant deny, got %v", err)
	}
	if _, _, err := f.svc.DownloadDocument(ctx, sp, upload.Document.ID); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("expected same-tenant document deny, got %v", err)
	}

	// platform.admin sees tenant resources through the scope, not a flag.
	admin := auth.Principal{TenantID: f.tenant, ActorID: "admin_test", ActorKind: auth.ActorKindUser,
		Scopes: map[string]struct{}{"platform.admin": {}}}
	if _, err := f.svc.GetCollection(ctx, admin, c.ID); err != nil {
		t.Fatalf("expected admin read, got %v", err)
	}
}

func TestEmbedFenceStaleAttemptCannotClobberNewerVersion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)

	uploadA, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName:   "a.txt",
		MimeType:   "text/plain",
		Bytes:      []byte("version-A-unique-text"),
		SourceType: "slack",
		SourceID:   "thread-1",
	})
	if err != nil {
		t.Fatalf("upload A: %v", err)
	}
	// Version B lands before A's job ever runs — A's fenced attempt must
	// mark its generation superseded and never index stale chunks.
	uploadB, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName:   "b.txt",
		MimeType:   "text/plain",
		Bytes:      []byte("version-B-unique-text"),
		SourceType: "slack",
		SourceID:   "thread-1",
	})
	if err != nil {
		t.Fatalf("upload B: %v", err)
	}
	if uploadB.Document.ID != uploadA.Document.ID {
		t.Fatalf("expected stable document identity, got %s vs %s", uploadA.Document.ID, uploadB.Document.ID)
	}
	f.runNextJob(t) // A's job — must supersede
	f.runNextJob(t) // B's job — publishes
	doc := f.docStatus(t, c.ID, uploadA.Document.ID)
	if doc.Status != rag.DocumentStatusReady {
		t.Fatalf("expected ready, got %s", doc.Status)
	}
	for _, point := range f.vector.points[c.QdrantCollection] {
		content, _ := point.Payload["content"].(string)
		if strings.Contains(content, "version-A-unique-text") {
			t.Fatalf("stale attempt published version A content: %q", content)
		}
	}
	results, err := f.svc.Search(ctx, f.principal, rag.SearchInput{CollectionID: c.ID, Query: "version-B", Limit: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 || !strings.Contains(results[0].Content, "version-B-unique-text") {
		t.Fatalf("expected version B indexed, got %#v", results)
	}
}

func TestReuploadSameSourceIsNewVersionNotDuplicate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)
	uploadA, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "v.txt", MimeType: "text/plain", Bytes: []byte("rev-A"), SourceType: "github", SourceID: "github:acme/docs:f.md",
	})
	if err != nil {
		t.Fatalf("upload A: %v", err)
	}
	f.runNextJob(t)
	uploadB, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "v.txt", MimeType: "text/plain", Bytes: []byte("rev-B"), SourceType: "github", SourceID: "github:acme/docs:f.md",
	})
	if err != nil {
		t.Fatalf("upload B: %v", err)
	}
	f.runNextJob(t)
	if uploadB.Document.ID != uploadA.Document.ID {
		t.Fatalf("expected same object identity, got %s vs %s", uploadA.Document.ID, uploadB.Document.ID)
	}
	doc := f.docStatus(t, c.ID, uploadB.Document.ID)
	if doc.ObservedSeq != 2 {
		t.Fatalf("expected observed_seq 2, got %d", doc.ObservedSeq)
	}
}
