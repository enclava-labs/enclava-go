package rag_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func (f *fakeVectorStore) DeleteGenerationPoints(ctx context.Context, collection, versionID string, generation int64) error {
	kept := f.points[collection][:0]
	for _, point := range f.points[collection] {
		if point.Payload["object_version_id"] == versionID && point.Payload["generation"] == generation {
			continue
		}
		kept = append(kept, point)
	}
	f.points[collection] = kept
	return nil
}

func (f *fakeVectorStore) SetPayload(ctx context.Context, collection string, filter map[string]any, payload map[string]any) error {
	for i := range f.points[collection] {
		for key, value := range payload {
			f.points[collection][i].Payload[key] = value
		}
	}
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
func (f *fakeVectorStore) Query(ctx context.Context, collection string, input rag.QueryInput) ([]rag.QdrantSearchPoint, error) {
	return f.SearchHybrid(ctx, collection, input.DenseVector, rag.SparseVector{}, input.Limit, input.Filter)
}

type fixture struct {
	db        *postgres.DB
	svc       *rag.Service
	repo      *postgres.RAGRepo
	jobs      *jobs.Service
	store     *artifacts.Store
	vector    *fakeVectorStore
	embedding *fakeEmbeddingUpstream
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
	embedding := &fakeEmbeddingUpstream{}
	svc := rag.NewService(repo, vector, newExecutor(t, db, tenant, embedding), "embed-test", store, jobsSvc)
	p := auth.Principal{
		TenantID:            tenant,
		ActorID:             user.ID,
		ActorKind:           auth.ActorKindUser,
		UserID:              user.ID,
		CredentialID:        "key_test",
		CredentialKind:      auth.CredentialKindAPIKey,
		Scopes:              map[string]struct{}{"rag": {}, "rag.manage": {}},
		AllowedModelsAll:    true,
		AllowedEndpointsAll: true,
	}
	return &fixture{db: db, svc: svc, repo: repo, jobs: jobsSvc, store: store, vector: vector, embedding: embedding, tenant: tenant, principal: p}
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
	docs, err := f.svc.ListDocuments(context.Background(), f.principal, collectionID, rag.DocumentStatusAll, 100, 0)
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
	if results[0].EvidenceID == "" {
		t.Fatal("retrieval result has no server-minted evidence ID")
	}
	resolved, err := f.svc.ResolveEvidence(ctx, f.principal, []string{results[0].EvidenceID})
	if err != nil || len(resolved) != 1 || resolved[0].Status != "current" || resolved[0].RetainedCopyLabel != "retained copy" {
		t.Fatalf("resolve current evidence: %#v err=%v", resolved, err)
	}
	if _, err := f.svc.ResolveEvidence(ctx, f.principal, []string{"forged-evidence-id"}); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("forged evidence should use the not-found outcome, got %v", err)
	}
	contextResult, err := f.svc.BuildContext(ctx, f.principal, results, 100)
	if err != nil || contextResult.CharacterLimit != 100 || !strings.Contains(contextResult.Text, results[0].EvidenceID) {
		t.Fatalf("build evidence context: %#v err=%v", contextResult, err)
	}
	copyBytes, err := f.svc.DownloadEvidenceCopy(ctx, f.principal, results[0].EvidenceID)
	if err != nil || string(copyBytes) != "enclava rag search content for integration testing" {
		t.Fatalf("authorized retained copy download: %q err=%v", copyBytes, err)
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

func TestLexicalSearchSkipsEmbeddingAndRechecksRevocation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)
	upload, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{FileName: "secret.txt", MimeType: "text/plain", Bytes: []byte("private-copper-aster content")})
	if err != nil {
		t.Fatal(err)
	}
	f.runNextJob(t)
	calls := f.embedding.calls
	results, err := f.svc.Search(ctx, f.principal, rag.SearchInput{CollectionID: c.ID, Query: "private copper", Mode: rag.QueryLexical})
	if err != nil || len(results) == 0 {
		t.Fatalf("lexical-only retrieval: %#v err=%v", results, err)
	}
	if f.embedding.calls != calls {
		t.Fatalf("lexical-only search called embedding executor: before=%d after=%d", calls, f.embedding.calls)
	}
	if _, err := f.db.SQL.ExecContext(ctx, `UPDATE source_objects SET audience='[]'::jsonb WHERE tenant_id=$1 AND id=$2`, f.tenant, upload.Document.ID); err != nil {
		t.Fatal(err)
	}
	revoked, err := f.svc.Search(ctx, f.principal, rag.SearchInput{CollectionID: c.ID, Query: "private copper", Mode: rag.QueryLexical})
	if err != nil || len(revoked) != 0 {
		t.Fatalf("revoked result leaked: %#v err=%v", revoked, err)
	}
	contextResult, err := f.svc.BuildContext(ctx, f.principal, results, 200)
	if err != nil || strings.Contains(contextResult.Text, "private-copper-aster") || !contextResult.NeighborWithheld {
		t.Fatalf("revoked context was not withheld: %#v err=%v", contextResult, err)
	}
	if _, err := f.svc.ResolveEvidence(ctx, f.principal, []string{results[0].EvidenceID}); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("revoked evidence did not resolve as not found: %v", err)
	}
}

func TestOptionalRewriteFailureFallsBackToDirectSearch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)
	upload, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{FileName: "direct.txt", MimeType: "text/plain", Bytes: []byte("original-query direct retrieval")})
	if err != nil {
		t.Fatal(err)
	}
	f.runNextJob(t)
	results, err := f.svc.Search(ctx, f.principal, rag.SearchInput{CollectionID: c.ID, Query: "original-query", Rewrite: true, RewriteModel: "embed-test", SelectSections: true, SectionModel: "embed-test"})
	if err != nil || len(results) == 0 || results[0].DocumentID != upload.Document.ID {
		t.Fatalf("rewrite failure should retain direct search: %#v err=%v", results, err)
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
	points := f.vector.points[c.QdrantCollection]
	if len(points) == 0 {
		t.Fatal("current generation points are missing")
	}
	for _, point := range points {
		if point.Payload["object_version_id"] != doc.ObjectVersionID {
			t.Fatalf("orphan point survived publication: %#v", point.Payload)
		}
	}
	var pending int
	if err := f.db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM index_reconciliation WHERE tenant_id=$1 AND state='pending'`, f.tenant).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending reconciliations = %d, err = %v", pending, err)
	}
}

func TestKnowledgeImportReservationSurvivesCrashAfterObjectCreation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	collection := f.collection(t)
	reservedID, complete, err := f.repo.ReserveKnowledgeImport(ctx, f.tenant, collection.ID, "crashed-result", "ragdoc_first", "generated")
	if err != nil || complete || reservedID != "ragdoc_first" {
		t.Fatalf("first reservation = %q %v %v", reservedID, complete, err)
	}
	object := source.Object{
		TenantID: f.tenant, ID: reservedID, Kind: "generated", NativeID: "crashed-result",
		AuthorizationMode: source.AuthorizationAudience, OwnerActorID: f.principal.ActorID, Audience: []string{f.principal.ActorID},
	}
	if _, err := f.repo.UpsertObject(ctx, object); err != nil {
		t.Fatalf("create imported object: %v", err)
	}

	// Simulate process death after object creation but before RecordKnowledgeImport.
	retryID, complete, err := f.repo.ReserveKnowledgeImport(ctx, f.tenant, collection.ID, "crashed-result", "ragdoc_second", "generated")
	if err != nil || complete || retryID != reservedID {
		t.Fatalf("retry reservation = %q %v %v", retryID, complete, err)
	}
	object.ID = retryID
	if _, err := f.repo.UpsertObject(ctx, object); err != nil {
		t.Fatalf("retry imported object: %v", err)
	}
	var count int
	if err := f.db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM source_objects WHERE tenant_id=$1 AND id=$2`, f.tenant, reservedID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("objects after retry = %d, err = %v", count, err)
	}
	if err := f.repo.RecordKnowledgeImport(ctx, f.tenant, collection.ID, "crashed-result", reservedID, "generated"); err != nil {
		t.Fatal(err)
	}
	retryID, complete, err = f.repo.ReserveKnowledgeImport(ctx, f.tenant, collection.ID, "crashed-result", "ragdoc_third", "generated")
	if err != nil || !complete || retryID != reservedID {
		t.Fatalf("completed reservation = %q %v %v", retryID, complete, err)
	}
}

func TestImportArtifactInheritsSourceRestrictions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	collection := f.collection(t)
	writer, _, err := f.store.Begin(ctx, artifacts.Meta{
		Ref: artifacts.Ref{TenantID: f.tenant, Kind: artifacts.KindOriginal}, MimeType: "text/plain",
	}, time.Minute, 1024)
	if err != nil {
		t.Fatal(err)
	}
	const body = "restricted source"
	if _, err := writer.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Finalize(ctx); err != nil {
		t.Fatal(err)
	}
	ref := writer.Ref()
	object, err := f.repo.UpsertObject(ctx, source.Object{
		TenantID: f.tenant, ID: "source_import_fixture", Kind: "upload", NativeID: "import-source",
		AuthorizationMode: source.AuthorizationAudience, OwnerActorID: f.principal.ActorID, Audience: []string{f.principal.ActorID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.InsertObjectVersion(ctx, f.tenant, source.ObjectVersion{
		ObjectID: object.ID, ObservedSeq: 1, ArtifactRef: ref.ID, SHA256: ref.SHA256,
		ByteSize: int64(len(body)), MimeType: "text/plain", OriginalFilename: "restricted.txt",
	}); err != nil {
		t.Fatal(err)
	}
	restriction := source.SourceRestriction{
		TenantID: f.tenant, SubjectKind: source.RestrictionObject, SubjectID: object.ID,
		DeniedCapabilities: []string{inference.CapabilityEmbeddings},
	}
	if err := f.repo.SetRestriction(ctx, restriction); err != nil {
		t.Fatal(err)
	}
	out, err := f.svc.ImportArtifact(ctx, f.principal, rag.ImportArtifactInput{CollectionID: collection.ID, ArtifactRef: ref.ID})
	if err != nil {
		t.Fatal(err)
	}
	version, err := f.repo.GetObjectVersion(ctx, f.tenant, out.Document.ID, out.Document.ObservedSeq)
	if err != nil {
		t.Fatal(err)
	}
	if len(version.Restrictions) != 1 || len(version.Restrictions[0].DeniedCapabilities) != 1 ||
		version.Restrictions[0].DeniedCapabilities[0] != inference.CapabilityEmbeddings {
		t.Fatalf("imported restrictions = %+v", version.Restrictions)
	}
}

// runNextJobExpectingFailure leases the next queued job, runs the embed
// handler (which must error) and settles the job failed — the durable
// failure the document projection must surface truthfully.
func (f *fixture) runNextJobExpectingFailure(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	claim, err := f.jobs.Lease(ctx, "test-worker", time.Minute)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if claim == nil {
		t.Fatal("no job to lease")
	}
	if _, err := f.svc.JobHandler()(ctx, claim, nil); err == nil {
		_ = claim.Fail(ctx, errors.New("expected failure"), jobs.FailureFatal)
		t.Fatal("expected embed attempt to fail")
	} else if failErr := claim.Fail(ctx, err, jobs.FailureFatal); failErr != nil {
		t.Fatalf("settle failed job: %v", failErr)
	}
}

func newScopedUser(t *testing.T, f *fixture, email string, scopes ...string) auth.Principal {
	t.Helper()
	user := testdb.SeedUser(t, f.db, f.tenant, email, nil)
	scopesMap := make(map[string]struct{}, len(scopes))
	for _, s := range scopes {
		scopesMap[s] = struct{}{}
	}
	return auth.Principal{TenantID: f.tenant, ActorID: user.ID, UserID: user.ID, ActorKind: auth.ActorKindUser, Scopes: scopesMap}
}

func TestServiceListDocumentsAuthorizedPagination(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)
	member := newScopedUser(t, f, "member-list@example.test", "rag")
	manager := newScopedUser(t, f, "manager-list@example.test", "rag", "rag.manage")

	// Three shared documents (explicit member audience) interleaved with two
	// restricted ones (uploader-only audience). Object audiences match exact
	// actor IDs — collection visibility never widens source reads.
	shared, restricted := []string{}, []string{}
	for i, spec := range []struct {
		name     string
		audience []string
		ids      *[]string
	}{
		{"shared-a.txt", []string{f.principal.ActorID, member.ActorID}, &shared},
		{"restricted-a.txt", []string{f.principal.ActorID}, &restricted},
		{"shared-b.txt", []string{f.principal.ActorID, member.ActorID}, &shared},
		{"shared-c.txt", []string{f.principal.ActorID, member.ActorID}, &shared},
		{"restricted-b.txt", []string{f.principal.ActorID}, &restricted},
	} {
		out, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
			FileName: spec.name, MimeType: "text/plain",
			Bytes: []byte(fmt.Sprintf("enclava document %d content", i)), Audience: spec.audience,
		})
		if err != nil {
			t.Fatalf("upload %s: %v", spec.name, err)
		}
		*spec.ids = append(*spec.ids, out.Document.ID)
	}

	listIDs := func(p auth.Principal, status string, limit, offset int) []string {
		t.Helper()
		docs, err := f.svc.ListDocuments(ctx, p, c.ID, status, limit, offset)
		if err != nil {
			t.Fatalf("list documents: %v", err)
		}
		ids := make([]string, 0, len(docs))
		for _, d := range docs {
			ids = append(ids, d.ID)
		}
		return ids
	}

	// Member pagination describes the AUTHORIZED set: denied rows never
	// consume page slots and no restricted title leaks.
	seen := map[string]bool{}
	for _, offset := range []int{0, 2, 4} {
		page := listIDs(member, rag.DocumentStatusAll, 2, offset)
		if len(page) > 2 {
			t.Fatalf("page larger than limit: %#v", page)
		}
		for _, id := range page {
			if seen[id] {
				t.Fatalf("document %s repeated across pages", id)
			}
			seen[id] = true
			for _, denied := range restricted {
				if id == denied {
					t.Fatalf("restricted document %s leaked to member page %d", id, offset)
				}
			}
		}
	}
	if len(seen) != len(shared) {
		t.Fatalf("member pages saw %d documents, want the %d shared ones: %#v", len(seen), len(shared), seen)
	}
	if beyond := listIDs(member, rag.DocumentStatusAll, 2, len(shared)); len(beyond) != 0 {
		t.Fatalf("authorized pagination ran past the authorized set: %#v", beyond)
	}

	// rag.manage never bypasses source authorization: a manage-scoped caller
	// without audience membership sees nothing — no titles, no counts.
	if got := listIDs(manager, rag.DocumentStatusAll, 50, 0); len(got) != 0 {
		t.Fatalf("manage-scoped caller saw %d unauthorized documents: %#v", len(got), got)
	}
	// The owner (and audience member) sees everything.
	if got := listIDs(f.principal, rag.DocumentStatusAll, 50, 0); len(got) != len(shared)+len(restricted) {
		t.Fatalf("owner saw %d documents, want %d: %#v", len(got), len(shared)+len(restricted), got)
	}

	// Unknown statuses are caller errors, and removed rows are unknown to
	// non-managers (silently, not an error).
	if _, err := f.svc.ListDocuments(ctx, f.principal, c.ID, "bogus", 10, 0); !errors.Is(err, rag.ErrInvalidInput) {
		t.Fatalf("unknown status must be ErrInvalidInput, got %v", err)
	}
	if removed := listIDs(member, rag.DocumentStatusRemoved, 10, 0); len(removed) != 0 {
		t.Fatalf("removed rows leaked to member: %#v", removed)
	}

	// Soft delete: the member's set shrinks, the management Removed view
	// carries tombstone metadata only, and re-reading is denied.
	if err := f.svc.DeleteDocument(ctx, f.principal, shared[0]); err != nil {
		t.Fatalf("delete document: %v", err)
	}
	if got := listIDs(member, rag.DocumentStatusAll, 50, 0); len(got) != len(shared)-1 {
		t.Fatalf("member still lists removed document: %#v", got)
	}
	removedDocs, err := f.svc.ListDocuments(ctx, f.principal, c.ID, rag.DocumentStatusRemoved, 10, 0)
	if err != nil || len(removedDocs) != 1 {
		t.Fatalf("management removed view: %v %#v", err, removedDocs)
	}
	if removedDocs[0].Status != rag.DocumentStatusRemoved || removedDocs[0].TextHash != "" ||
		removedDocs[0].QdrantPointCount != 0 || len(removedDocs[0].Metadata) != 0 {
		t.Fatalf("removed view must be metadata-only: %#v", removedDocs[0])
	}
	if _, _, err := f.svc.DownloadDocument(ctx, f.principal, shared[0]); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("removed document must read as absent, got %v", err)
	}
}

func TestServiceFailedDocumentsAndCountAreAuthorized(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)

	member := newScopedUser(t, f, "member-fail@example.test", "rag")
	manager := newScopedUser(t, f, "manager-fail@example.test", "rag", "rag.manage")
	// One restricted failure (uploader-only audience) and one shared failure
	// (member audience); whitespace-only payloads fail the embed with "no
	// extractable text".
	_, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "restricted-fail.txt", MimeType: "text/plain",
		Bytes: []byte("   \n"), Audience: []string{f.principal.ActorID},
	})
	if err != nil {
		t.Fatalf("upload restricted failure: %v", err)
	}
	sharedDoc, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "shared-fail.txt", MimeType: "text/plain", Bytes: []byte("   \n"),
		Audience: []string{f.principal.ActorID, member.ActorID},
	})
	if err != nil {
		t.Fatalf("upload shared failure: %v", err)
	}
	f.runNextJobExpectingFailure(t)
	f.runNextJobExpectingFailure(t)

	failedDocs, err := f.svc.ListDocuments(ctx, f.principal, c.ID, rag.DocumentStatusFailed, 10, 0)
	if err != nil || len(failedDocs) != 2 {
		t.Fatalf("owner failed list: %v %#v", err, failedDocs)
	}
	for _, d := range failedDocs {
		if d.Status != rag.DocumentStatusFailed || d.ErrorMessage == "" {
			t.Fatalf("failed row must carry the job error: %#v", d)
		}
	}
	if ready, err := f.svc.ListDocuments(ctx, f.principal, c.ID, rag.DocumentStatusReady, 10, 0); err != nil || len(ready) != 0 {
		t.Fatalf("failed rows must not surface as ready: %v %#v", err, ready)
	}

	// Authorized failure aggregates: the restricted failure is absent for the
	// member AND for the manage-scoped caller — rag.manage never bypasses,
	// and without any audience membership the manager sees no failure at all.
	assertAuthorizedFailures := func(p auth.Principal, wantID string) {
		t.Helper()
		failed, err := f.svc.ListFailedDocuments(ctx, p, 10)
		if err != nil {
			t.Fatalf("list failed documents: %v", err)
		}
		if len(failed) != 1 || failed[0].ID != wantID {
			t.Fatalf("authorized failures = %#v, want only %s", failed, wantID)
		}
	}
	assertAuthorizedFailures(member, sharedDoc.Document.ID)
	managerFailed, err := f.svc.ListFailedDocuments(ctx, manager, 10)
	if err != nil || len(managerFailed) != 0 {
		t.Fatalf("manage-scoped caller must see no unauthorized failures: %v %#v", err, managerFailed)
	}
	ownerFailed, err := f.svc.ListFailedDocuments(ctx, f.principal, 10)
	if err != nil || len(ownerFailed) != 2 {
		t.Fatalf("owner failures: %v %#v", err, ownerFailed)
	}

	// The authorized document count matches the authorized list for every
	// role — restricted rows never count.
	count, err := f.svc.CountDocuments(ctx, member)
	if err != nil || count != 1 {
		t.Fatalf("member count = %d, %v (want the 1 shared document)", count, err)
	}
	managerCount, err := f.svc.CountDocuments(ctx, manager)
	if err != nil || managerCount != 0 {
		t.Fatalf("manage-scoped count = %d, %v; manage must not bypass source ACLs", managerCount, err)
	}
	ownerCount, err := f.svc.CountDocuments(ctx, f.principal)
	if err != nil || ownerCount != 2 {
		t.Fatalf("owner count = %d, %v (want 2)", ownerCount, err)
	}
}

// ---------------------------------------------------------------------
// 08-08: bounded readable search, reader upload, extraction import, profile
// freeze.

// refRecordingExecutor records the input-ref fan-out of every embedding
// request — the executor Inputs slice is the billing/authorization surface.
type refRecordingExecutor struct {
	refCounts []int
}

func (e *refRecordingExecutor) Execute(ctx context.Context, p auth.Principal, req inference.Request) (*inference.Result, error) {
	e.refCounts = append(e.refCounts, len(req.Inputs))
	body := `{"object":"list","data":[{"index":0,"embedding":[1,2,3]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`
	return &inference.Result{Body: io.NopCloser(strings.NewReader(body))}, nil
}

// stageSearchDoc stages an authorized published document with one chunk and
// registers its point with the fake vector store.
func (f *fixture) stageSearchDoc(t *testing.T, collection rag.Collection, objectID, content, provenance string) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.repo.UpsertObject(ctx, source.Object{
		TenantID: f.tenant, ID: objectID, Kind: "document", NativeID: "native-" + objectID,
		AuthorizationMode: source.AuthorizationAudience, OwnerActorID: f.principal.ActorID,
		Audience: []string{f.principal.ActorID, "*"},
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
			{ID: "blk_" + objectID, Ordinal: 0, Kind: "paragraph", Text: content, ByteStart: 0, ByteEnd: len(content),
				Locator: source.Locator{Page: new(3)}},
		},
	}
	if err := f.repo.SaveParsedDocument(ctx, "rag-structured-v1", parsed); err != nil {
		t.Fatal(err)
	}
	profileID, err := f.repo.EnsureChunkProfile(ctx, f.tenant, "rag-"+collection.ID+"-structured", 200, 20)
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
	if err := f.vector.UpsertPoints(ctx, collection.QdrantCollection, []rag.QdrantPoint{{
		ID: "pt_" + objectID, Payload: map[string]any{
			"tenant_id": f.tenant, "document_id": objectID, "object_version_id": objectID + "#1",
			"generation": int64(1), "chunk_id": chunkID, "file_name": objectID + ".txt",
			"source_uri": "urn:test:" + objectID, "content": content, "access_enabled": true,
		},
	}}); err != nil {
		t.Fatal(err)
	}
}

// TestSearchReadableAuthorizedFusionAndFacets: denied collections contribute
// neither results nor facets; explicit caller-supplied IDs are denied, never
// silently filtered; results carry canonical source kinds, collection names
// and resolved locations; facet counts equal the returned composition.
func TestSearchReadableAuthorizedFusionAndFacets(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	allowed, err := f.svc.CreateCollection(ctx, f.principal, rag.CreateCollectionInput{Name: "allowed", ACL: map[string]any{"public": true}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.svc.CreateCollection(ctx, f.principal, rag.CreateCollectionInput{Name: "second", ACL: map[string]any{"public": true}})
	if err != nil {
		t.Fatal(err)
	}
	outsider := auth.Principal{TenantID: f.tenant, ActorID: "actor_other", UserID: "actor_other", ActorKind: auth.ActorKindUser}
	denied, err := f.svc.CreateCollection(ctx, outsider, rag.CreateCollectionInput{Name: "denied"})
	if err != nil {
		t.Fatal(err)
	}
	f.stageSearchDoc(t, allowed, "obj_allow", "fusion readable alpha", `{"kind":"upload"}`)
	f.stageSearchDoc(t, second, "obj_second", "fusion readable beta", `{"source_kind":"notion"}`)
	f.stageSearchDoc(t, denied, "obj_secret", "fusion secret gamma", `{}`)

	result, err := f.svc.SearchReadable(ctx, f.principal, "fusion readable", rag.QueryLexical, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 2 {
		t.Fatalf("results=%d, want 2 with the denied collection excluded: %#v", len(result.Results), result.Results)
	}
	kinds := map[string]string{}
	for _, r := range result.Results {
		kinds[r.CollectionID] = r.SourceKind
		if r.CollectionName == "" || r.SourceKind == "" || r.EvidenceID == "" {
			t.Fatalf("result missing presentation fields: %#v", r)
		}
		if len(r.Locations) == 0 || r.Locations[0].Page == nil || *r.Locations[0].Page != 3 {
			t.Fatalf("locations not resolved from retained blocks: %#v", r.Locations)
		}
	}
	if kinds[allowed.ID] != "upload" || kinds[second.ID] != "notion" {
		t.Fatalf("source kinds must come from provenance, not object.Kind: %#v", kinds)
	}
	if len(result.Facets) != 2 {
		t.Fatalf("facets=%#v, want upload+notion over the authorized set", result.Facets)
	}

	if _, err := f.svc.SearchReadable(ctx, f.principal, "fusion", rag.QueryLexical, []string{denied.ID}, 5); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("explicit denied collection = %v, want ErrDenied", err)
	}
	if _, err := f.svc.SearchReadable(ctx, f.principal, "fusion", rag.QueryLexical, []string{"ragcol_missing"}, 5); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("explicit unknown collection = %v, want ErrDenied", err)
	}
	narrow, err := f.svc.SearchReadable(ctx, f.principal, "fusion readable", rag.QueryLexical, []string{second.ID}, 20)
	if err != nil || len(narrow.Results) != 1 || narrow.Results[0].CollectionID != second.ID {
		t.Fatalf("explicit readable selection = %#v err=%v", narrow, err)
	}
	if len(narrow.Facets) != 1 || narrow.Facets[0].SourceKind != "notion" || narrow.Facets[0].Count != 1 {
		t.Fatalf("explicit selection facets=%#v", narrow.Facets)
	}
}

// TestSearchReadableLimitBoundsFacets: facet counts describe exactly the
// returned, limit-bounded composition.
func TestSearchReadableLimitBoundsFacets(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	collection, err := f.svc.CreateCollection(ctx, f.principal, rag.CreateCollectionInput{Name: "limitless", ACL: map[string]any{"public": true}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		f.stageSearchDoc(t, collection, fmt.Sprintf("obj_lim_%d", i), fmt.Sprintf("limit content %d", i), `{"kind":"upload"}`)
	}
	result, err := f.svc.SearchReadable(ctx, f.principal, "limit content", rag.QueryLexical, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 2 || len(result.Facets) != 1 || result.Facets[0].Count != 2 || result.Facets[0].SourceKind != "upload" {
		t.Fatalf("bounded results=%d facets=%#v, want 2 results and one upload facet of 2", len(result.Results), result.Facets)
	}
}

// TestSearchWorkBoundsAndRefFanOut: the object/work bound is explicit (never
// silent first-N) and the executor input-ref fan-out stays within the
// authorized-candidate bound.
func TestSearchWorkBoundsAndRefFanOut(t *testing.T) {
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "rag-bounds")
	user := testdb.SeedUser(t, db, tenant, "rag-bounds@example.test", nil)
	repo := postgres.NewRAGRepo(db)
	vector := newFakeVectorStore()
	recorder := &refRecordingExecutor{}
	svc := rag.NewService(repo, vector, recorder, "embed-test", nil, nil)
	p := auth.Principal{TenantID: tenant, ActorID: user.ID, UserID: user.ID, ActorKind: auth.ActorKindUser,
		Scopes: map[string]struct{}{"rag": {}, "rag.manage": {}}}
	f := &fixture{db: db, svc: svc, repo: repo, vector: vector, tenant: tenant, principal: p}
	ctx := context.Background()
	collection, err := svc.CreateCollection(ctx, p, rag.CreateCollectionInput{Name: "bounded", ACL: map[string]any{"public": true}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		f.stageSearchDoc(t, collection, fmt.Sprintf("obj_w_%d", i), fmt.Sprintf("bounded work %d", i), `{}`)
	}
	previous := rag.SearchWorkBudget
	defer func() { rag.SearchWorkBudget = previous }()
	rag.SearchWorkBudget = 2

	// Over budget → explicit error on the single-collection path...
	if _, err := svc.Search(ctx, p, rag.SearchInput{CollectionID: collection.ID, Query: "bounded", Mode: rag.QueryHybrid}); !errors.Is(err, rag.ErrSearchWorkExceeded) {
		t.Fatalf("over-budget search = %v, want ErrSearchWorkExceeded", err)
	}
	// ...and on the cross-collection path.
	if _, err := svc.SearchReadable(ctx, p, "bounded", rag.QueryHybrid, nil, 5); !errors.Is(err, rag.ErrSearchWorkExceeded) {
		t.Fatalf("over-budget readable search = %v, want ErrSearchWorkExceeded", err)
	}
	if len(recorder.refCounts) != 0 {
		t.Fatalf("over-budget request must not reach the executor: %#v", recorder.refCounts)
	}

	// Under budget, the search is complete and the executor input-ref
	// fan-out is exactly the authorized candidate set.
	rag.SearchWorkBudget = 10
	results, err := svc.Search(ctx, p, rag.SearchInput{CollectionID: collection.ID, Query: "bounded", Mode: rag.QueryHybrid})
	if err != nil || len(results) != 3 {
		t.Fatalf("under-budget search results=%d err=%v", len(results), err)
	}
	if len(recorder.refCounts) != 1 || recorder.refCounts[0] != 3 {
		t.Fatalf("executor ref fan-out = %#v, want one call carrying 3 refs", recorder.refCounts)
	}

	// The aggregate budget binds across collections of one request: with a
	// budget of 3 total, a 3-document collection consumes it and any further
	// collection becomes an explicit error, not silent omission.
	rag.SearchWorkBudget = 3
	extra, err := svc.CreateCollection(ctx, p, rag.CreateCollectionInput{Name: "extra", ACL: map[string]any{"public": true}})
	if err != nil {
		t.Fatal(err)
	}
	f.stageSearchDoc(t, extra, "obj_extra", "bounded extra work", `{}`)
	if _, err := svc.SearchReadable(ctx, p, "bounded", rag.QueryHybrid, nil, 10); !errors.Is(err, rag.ErrSearchWorkExceeded) {
		t.Fatalf("aggregate budget = %v, want ErrSearchWorkExceeded", err)
	}
	if total := recorder.refCounts[len(recorder.refCounts)-1]; total > 3 {
		t.Fatalf("per-call ref fan-out %d exceeded the bound", total)
	}
}

// TestServiceUploadReaderStreamsAndPersistsRefMetadata: the Reader path
// streams into the same artifact writer, and the persisted version row
// carries the finalized ref's real digest and size.
func TestServiceUploadReaderStreamsAndPersistsRefMetadata(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)
	content := "enclava reader streamed upload content"
	upload, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "reader.txt", MimeType: "text/plain", Reader: strings.NewReader(content),
	})
	if err != nil {
		t.Fatalf("reader upload: %v", err)
	}
	var sha256Hex string
	var byteSize int64
	if err := f.db.SQL.QueryRowContext(ctx, `SELECT content_sha256, byte_size FROM source_object_versions WHERE tenant_id=$1 AND object_id=$2`,
		f.tenant, upload.Document.ID).Scan(&sha256Hex, &byteSize); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	if sha256Hex != hex.EncodeToString(sum[:]) || byteSize != int64(len(content)) {
		t.Fatalf("persisted version metadata sha=%s size=%d, want the streamed bytes' digest/%d", sha256Hex, byteSize, len(content))
	}
	_, blob, err := f.svc.DownloadDocument(ctx, f.principal, upload.Document.ID)
	if err != nil || string(blob) != content {
		t.Fatalf("retained artifact round-trip: %q err=%v", blob, err)
	}
	// The enqueued embed job references the real object version and carries
	// the frozen profile version.
	job, err := f.jobs.Lease(ctx, "test-worker", time.Minute)
	if err != nil || job == nil {
		t.Fatalf("no embed job enqueued: %v", err)
	}
	if job.Job().InputRef != fmt.Sprintf("%s#1", upload.Document.ID) {
		t.Fatalf("embed job input ref = %q", job.Job().InputRef)
	}
	if job.Job().Policy["chunk_profile_version_id"] != c.ChunkProfileVersionID {
		t.Fatalf("embed job profile = %#v, want frozen %q", job.Job().Policy["chunk_profile_version_id"], c.ChunkProfileVersionID)
	}
	if _, err := f.svc.JobHandler()(ctx, job, nil); err != nil {
		t.Fatalf("worker on streamed upload: %v", err)
	}
	if _, err := job.Complete(ctx, fmt.Sprintf("%s#1", upload.Document.ID)); err != nil {
		t.Fatal(err)
	}
	if doc := f.docStatus(t, c.ID, upload.Document.ID); doc.Status != rag.DocumentStatusReady {
		t.Fatalf("streamed upload did not index: %s", doc.Status)
	}
}

// TestServiceUploadEmptyReaderLeavesNothing: the emptiness check runs on the
// FINALIZED artifact; the rejected upload deletes the finalized bytes and
// leaves no version row and no queued job.
func TestServiceUploadEmptyReaderLeavesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)
	if _, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "empty.txt", MimeType: "text/plain", Reader: strings.NewReader(""),
	}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty reader upload = %v, want an emptiness error", err)
	}
	var versions int
	if err := f.db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM source_object_versions WHERE tenant_id=$1`, f.tenant).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 0 {
		t.Fatalf("%d version rows survived an empty upload", versions)
	}
	var finals int
	if err := f.db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM artifacts WHERE tenant_id=$1 AND state='final'`, f.tenant).Scan(&finals); err != nil {
		t.Fatal(err)
	}
	if finals != 0 {
		t.Fatalf("%d final artifacts survived an empty upload", finals)
	}
	if claim, err := f.jobs.Lease(ctx, "test-worker", time.Minute); err != nil || claim != nil {
		t.Fatalf("empty upload enqueued work: claim=%v err=%v", claim, err)
	}
	// The Bytes path keeps its pre-finalize rejection semantics.
	if _, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{FileName: "b.txt", MimeType: "text/plain"}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty bytes upload = %v, want an emptiness error", err)
	}
}

// TestExtractionResultImportEndToEnd: a succeeded extract.process job's
// result artifact imports into a writable collection with preserved
// provenance; duplicates return the same document; non-owned and failed jobs
// are denied.
func TestExtractionResultImportEndToEnd(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)

	// The input artifact must still resolve to a readable source object.
	inputContent := "extract source document body"
	w, inputRef, err := f.store.Begin(ctx, artifacts.Meta{Ref: artifacts.Ref{TenantID: f.tenant, Kind: artifacts.KindOriginal}, MimeType: "text/plain"}, time.Minute, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(inputContent)); err != nil {
		t.Fatal(err)
	}
	if err := w.Finalize(ctx); err != nil {
		t.Fatal(err)
	}
	inputRef = w.Ref()
	if _, err := f.repo.UpsertObject(ctx, source.Object{TenantID: f.tenant, ID: "extract_source", Kind: "upload", NativeID: "extract-native",
		AuthorizationMode: source.AuthorizationOwner, OwnerActorID: f.principal.ActorID, ObservedSeq: 1}); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.InsertObjectVersion(ctx, f.tenant, source.ObjectVersion{ObjectID: "extract_source", ObservedSeq: 1,
		ArtifactRef: inputRef.ID, SHA256: inputRef.SHA256, ByteSize: inputRef.ByteSize, MimeType: "text/plain", OriginalFilename: "source.txt"}); err != nil {
		t.Fatal(err)
	}

	// A succeeded extract.process job with a stored JSON result artifact.
	resultDoc := []byte(`{"id":"res_1","data":{"summary":"extracted"}}`)
	rw, resultRef, err := f.store.Begin(ctx, artifacts.Meta{Ref: artifacts.Ref{TenantID: f.tenant, Kind: artifacts.KindResult}, MimeType: "application/json"}, time.Minute, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Write(resultDoc); err != nil {
		t.Fatal(err)
	}
	if err := rw.Finalize(ctx); err != nil {
		t.Fatal(err)
	}
	resultRef = rw.Ref()
	job, _, err := f.jobs.Enqueue(ctx, f.principal, jobs.EnqueueRequest{
		TenantID: f.tenant, Operation: "extract.process", IdempotencyKey: "extract-import-1",
		InputRef: inputRef.ID, Policy: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := f.jobs.Lease(ctx, "test-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("lease extraction job: %v", err)
	}
	if _, err := claim.Complete(ctx, resultRef.ID); err != nil {
		t.Fatal(err)
	}

	out, err := f.svc.ImportStoredResult(ctx, f.principal, rag.ImportResultInput{CollectionID: c.ID, ResultJobID: job.ID})
	if err != nil {
		t.Fatalf("extraction import: %v", err)
	}
	if out.Document.CollectionID != c.ID || out.Document.SourceType != "generated" || out.Document.FileName != "extraction-result.json" {
		t.Fatalf("imported document shape: %#v", out.Document)
	}
	duplicate, err := f.svc.ImportStoredResult(ctx, f.principal, rag.ImportResultInput{CollectionID: c.ID, ResultJobID: job.ID})
	if err != nil || duplicate.Document.ID != out.Document.ID {
		t.Fatalf("duplicate import = %#v err=%v, want the same document", duplicate, err)
	}

	// A job owned by someone else is indistinguishable from absent.
	other := auth.Principal{TenantID: f.tenant, ActorID: "actor_other", UserID: "actor_other", ActorKind: auth.ActorKindUser}
	if _, err := f.svc.ImportStoredResult(ctx, other, rag.ImportResultInput{CollectionID: c.ID, ResultJobID: job.ID}); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("non-owned import = %v, want ErrDenied", err)
	}
	// A failed job is denied.
	failed, _, err := f.jobs.Enqueue(ctx, f.principal, jobs.EnqueueRequest{
		TenantID: f.tenant, Operation: "extract.process", IdempotencyKey: "extract-import-2",
		InputRef: inputRef.ID, Policy: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	failedClaim, err := f.jobs.Lease(ctx, "test-worker", time.Minute)
	if err != nil || failedClaim == nil {
		t.Fatal("no failed-job claim")
	}
	if err := failedClaim.Fail(ctx, errors.New("boom"), jobs.FailureFatal); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ImportStoredResult(ctx, f.principal, rag.ImportResultInput{CollectionID: c.ID, ResultJobID: failed.ID}); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("failed-job import = %v, want ErrDenied", err)
	}
}

// TestProfileFreezeAcrossSettingsChange: the collection's immutable profile
// version is frozen into the queued embed job — switching the collection to a
// different existing version can never retcon the queued run.
func TestProfileFreezeAcrossSettingsChange(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	profiles := postgres.NewChunkProfilesRepo(f.db)
	profileA, err := profiles.Create(ctx, f.tenant, source.ChunkProfile{
		ProfileID: "profile-a", SizeCodePoints: 80, OverlapCodePoints: 10,
		BoundaryStrategy: "fixed", TableHandling: "row-wise",
	}, f.principal.ActorID)
	if err != nil {
		t.Fatal(err)
	}
	profileB, err := profiles.Create(ctx, f.tenant, source.ChunkProfile{
		ProfileID: "profile-b", SizeCodePoints: 2000, OverlapCodePoints: 10,
		BoundaryStrategy: "fixed", TableHandling: "row-wise",
	}, f.principal.ActorID)
	if err != nil {
		t.Fatal(err)
	}
	aID, bID := profileA.ID, profileB.ID
	c, err := f.svc.CreateCollection(ctx, f.principal, rag.CreateCollectionInput{
		Name: "frozen", ChunkProfileVersionID: &aID, ACL: map[string]any{"public": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.ChunkProfileVersionID != aID {
		t.Fatalf("create ignored chunk_profile_version_id: %q", c.ChunkProfileVersionID)
	}
	content := strings.Repeat("enclava freeze probe sentence. ", 12) // ~384 code points
	upload, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "freeze.txt", MimeType: "text/plain", Bytes: []byte(content),
	})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := f.jobs.Lease(ctx, "test-worker", time.Minute)
	if err != nil || queued == nil {
		t.Fatal("no queued embed job")
	}
	if queued.Job().Policy["chunk_profile_version_id"] != aID {
		t.Fatalf("queued job profile = %#v, want frozen A", queued.Job().Policy["chunk_profile_version_id"])
	}

	// Switch the collection to existing profile B AFTER enqueueing.
	updated, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{ChunkProfileVersionID: &bID})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ChunkProfileVersionID != bID {
		t.Fatalf("update ignored chunk_profile_version_id: %q", updated.ChunkProfileVersionID)
	}

	// Execute the queued worker: the produced chunks must reflect A's real
	// settings (80-code-point chunks), bound to A — never B.
	if _, err := f.svc.JobHandler()(ctx, queued, nil); err != nil {
		t.Fatalf("frozen-profile embed: %v", err)
	}
	if _, err := queued.Complete(ctx, fmt.Sprintf("%s#1", upload.Document.ID)); err != nil {
		t.Fatal(err)
	}
	var generationProfile string
	if err := f.db.SQL.QueryRowContext(ctx, `
		SELECT g.chunk_profile_version_id FROM index_generations g
		WHERE g.tenant_id=$1 AND g.collection_id=$2 AND g.object_version_id=$3`,
		f.tenant, c.ID, upload.Document.ID+"#1").Scan(&generationProfile); err != nil {
		t.Fatal(err)
	}
	if generationProfile != aID {
		t.Fatalf("generation bound to %q, want frozen A %q", generationProfile, aID)
	}
	chunks, err := f.repo.ListGenerationChunks(ctx, f.tenant, c.ID, upload.Document.ID+"#1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 3 {
		t.Fatalf("chunks=%d, want profile A's 80-code-point chunking (B would yield 1)", len(chunks))
	}
	for _, chunk := range chunks {
		if len([]rune(chunk.Content)) > 80 {
			t.Fatalf("chunk exceeds A's size: %d code points", len([]rune(chunk.Content)))
		}
	}

	// Unknown and foreign profile versions are caller errors.
	if _, err := f.svc.CreateCollection(ctx, f.principal, rag.CreateCollectionInput{
		Name: "broken", ChunkProfileVersionID: &[]string{"cpv_missing"}[0],
	}); !errors.Is(err, rag.ErrInvalidInput) {
		t.Fatalf("invalid profile version = %v, want ErrInvalidInput", err)
	}
	if _, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{
		ChunkProfileVersionID: &[]string{"cpv_missing"}[0],
	}); !errors.Is(err, rag.ErrInvalidInput) {
		t.Fatalf("invalid profile update = %v, want ErrInvalidInput", err)
	}
}
