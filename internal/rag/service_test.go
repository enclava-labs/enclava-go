package rag_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	calls  int
	inputs [][]string
	// midEmbed runs once inside the next embeddings call: a test's window
	// into an attempt that is still embedding.
	midEmbed func()
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
	if hook := f.midEmbed; hook != nil {
		f.midEmbed = nil
		hook()
	}
	var payload struct {
		Model string `json:"model"`
		Input any    `json:"input"`
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
	f.inputs = append(f.inputs, append([]string(nil), inputs...))
	data := make([]map[string]any, 0, len(inputs))
	for i, input := range inputs {
		vector := []float64{float64(len(input)), float64(strings.Count(input, "enclava") + 1), 1}
		if payload.Model == "embed-wide" { // a second, 40-dimension model to switch to
			for len(vector) < 40 {
				vector = append(vector, 0.5)
			}
		}
		data = append(data, map[string]any{"index": i, "embedding": vector})
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
	sources := postgres.NewSourceRepo(db)
	billingRepo := postgres.NewBillingRepo(db)
	addEmbeddingEndpoint(t, db, tenant, "ep-embed", "embed-test")
	svc, err := inference.NewService(inference.Deps{
		Endpoints:  postgres.NewEndpointsRepo(db),
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

// addEmbeddingEndpoint binds model on a priced embeddings endpoint served by
// the executor's fake transport.
func addEmbeddingEndpoint(t *testing.T, db *postgres.DB, tenant, endpointID, model string) {
	t.Helper()
	ctx := context.Background()
	if err := postgres.NewEndpointsRepo(db).Create(ctx, inference.Endpoint{
		ID:           endpointID,
		TenantID:     tenant,
		Protocol:     "openai_compatible",
		BaseOrigin:   "https://" + endpointID + ".upstream.example",
		Capabilities: []string{inference.CapabilityEmbeddings, "model:" + model},
		Enabled:      true,
	}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	billingRepo := postgres.NewBillingRepo(db)
	for _, unit := range []billing.UnitKind{billing.UnitInput, billing.UnitCachedInput} {
		if err := billingRepo.PutPrice(ctx, tenant, fmt.Sprintf("p-%s-%s-%s", endpointID, unit, newTestID()), billing.Price{
			SubjectType: billing.PriceSubjectEndpoint, SubjectID: endpointID,
			Capability: inference.CapabilityEmbeddings, Unit: unit,
			RateMicros: 1, UnitQuantity: 1, Currency: "USD",
		}, "tester", time.Now().UTC().Add(-time.Minute)); err != nil {
			t.Fatalf("put price: %v", err)
		}
	}
}

func newTestID() string { return fmt.Sprintf("t%d", time.Now().UnixNano()) }

type fakeVectorStore struct {
	points    map[string][]rag.QdrantPoint
	lastQuery []float64
	queried   []string // index names, in query order
	onPayload func()
	ensureErr error
}

func newFakeVectorStore() *fakeVectorStore {
	return &fakeVectorStore{points: map[string][]rag.QdrantPoint{}}
}

func (f *fakeVectorStore) EnsureCollection(ctx context.Context, collection string, vectorSize int) error {
	if f.ensureErr != nil {
		err := f.ensureErr
		f.ensureErr = nil
		return err
	}
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
	must, _ := filter["must"].([]map[string]any)
	for i := range f.points[collection] {
		matchPoint := true
		for _, clause := range must {
			match, _ := clause["match"].(map[string]any)
			key, _ := clause["key"].(string)
			if f.points[collection][i].Payload[key] != match["value"] {
				matchPoint = false
				break
			}
		}
		if !matchPoint {
			continue
		}
		for key, value := range payload {
			f.points[collection][i].Payload[key] = value
		}
	}
	if f.onPayload != nil {
		f.onPayload()
	}
	return nil
}

func (f *fakeVectorStore) CountPoints(_ context.Context, collection string) (int64, error) {
	return int64(len(f.points[collection])), nil
}

func (f *fakeVectorStore) DeleteCollection(_ context.Context, collection string) error {
	delete(f.points, collection)
	return nil
}

// SearchHybrid honors the access prefilter the service passes: tenant and
// access_enabled must match, acl must intersect the caller's tokens (or be
// absent — an unprojected legacy point), and published must be true or
// absent. Authorization itself is the service's PostgreSQL post-check.
func (f *fakeVectorStore) SearchHybrid(ctx context.Context, collection string, vector []float64, sparse rag.SparseVector, limit int, filter map[string]any) ([]rag.QdrantSearchPoint, error) {
	var tenant string
	var tokens []string
	if filter != nil {
		if must, ok := filter["must"].([]any); ok {
			for _, clause := range must {
				m, ok := clause.(map[string]any)
				if !ok {
					continue
				}
				if m["key"] == "tenant_id" {
					if match, ok := m["match"].(map[string]any); ok {
						tenant, _ = match["value"].(string)
					}
				}
				if should, ok := m["should"].([]any); ok {
					for _, alt := range should {
						am, ok := alt.(map[string]any)
						if !ok || am["key"] != "acl" {
							continue
						}
						if match, ok := am["match"].(map[string]any); ok {
							switch any := match["any"].(type) {
							case []string:
								tokens = any
							case []any:
								for _, v := range any {
									if s, ok := v.(string); ok {
										tokens = append(tokens, s)
									}
								}
							}
						}
					}
				}
			}
		}
	}
	out := []rag.QdrantSearchPoint{}
	for _, point := range f.points[collection] {
		payload := point.Payload
		if tenant != "" && payload["tenant_id"] != tenant {
			continue
		}
		if payload["access_enabled"] != true {
			continue
		}
		if acl, present := payload["acl"]; present {
			allowed := false
			var entries []string
			switch list := acl.(type) {
			case []string:
				entries = list
			case []any:
				for _, v := range list {
					if s, ok := v.(string); ok {
						entries = append(entries, s)
					}
				}
			}
			for _, entry := range entries {
				for _, token := range tokens {
					if entry == token {
						allowed = true
					}
				}
			}
			if !allowed {
				continue
			}
		}
		if published, present := payload["published"]; present && published != true {
			continue
		}
		out = append(out, rag.QdrantSearchPoint{ID: point.ID, Score: 0.99, Payload: payload})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeVectorStore) Query(ctx context.Context, collection string, input rag.QueryInput) ([]rag.QdrantSearchPoint, error) {
	f.lastQuery = input.DenseVector
	f.queried = append(f.queried, collection)
	points, err := f.SearchHybrid(ctx, collection, input.DenseVector, rag.SparseVector{}, input.Limit+input.Offset, input.Filter)
	if err != nil {
		return nil, err
	}
	if input.Offset >= len(points) {
		return nil, nil
	}
	return points[input.Offset:], nil
}

type fixture struct {
	db           *postgres.DB
	svc          *rag.Service
	repo         *postgres.RAGRepo
	jobs         *jobs.Service
	store        *artifacts.Store
	artifactRoot string
	vector       *fakeVectorStore
	embedding    *fakeEmbeddingUpstream
	executor     *inference.Service
	tenant       string
	principal    auth.Principal
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "rag")
	user := testdb.SeedUser(t, db, tenant, "rag-owner@example.test", nil)
	jobsRepo := postgres.NewJobsRepo(db)
	jobsSvc := jobs.NewService(jobsRepo, 0)
	artifactRepo := postgres.NewArtifactRepo(db)
	root := t.TempDir()
	store, err := artifacts.NewStore(root, t.TempDir(), artifactRepo)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	vector := newFakeVectorStore()
	repo := postgres.NewRAGRepo(db)
	embedding := &fakeEmbeddingUpstream{}
	executor := newExecutor(t, db, tenant, embedding)
	svc := rag.NewService(repo, vector, executor, "embed-test", store, jobsSvc)
	svc.SetIndexRetirementSoak(time.Nanosecond)
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
	return &fixture{db: db, svc: svc, repo: repo, jobs: jobsSvc, store: store, artifactRoot: root, vector: vector, embedding: embedding, executor: executor, tenant: tenant, principal: p}
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

// runNextJob processes any preceding scan jobs, then one non-scan job.
func (f *fixture) runNextJob(t *testing.T) {
	t.Helper()
	for {
		operation, _ := f.runOneJob(t)
		if operation != rag.OperationReembedScan {
			return
		}
	}
}

// runOneJob leases and settles the next durable job, reporting its operation
// and row ID so tests can prove a scan was re-enqueued rather than deduped.
func (f *fixture) runOneJob(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()
	claim, err := f.jobs.Lease(ctx, "test-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("lease: %v (claim %v)", err, claim)
	}
	operation := claim.Job().Operation
	handler := f.svc.JobHandler()
	if operation == rag.OperationReembedScan {
		handler = f.svc.ReembedScanJobHandler()
	}
	resultRef, err := handler(ctx, claim, nil)
	if err != nil {
		_ = claim.Fail(ctx, err, jobs.FailureFatal)
		t.Fatalf("%s attempt: %v", operation, err)
	}
	if _, err := claim.Complete(ctx, resultRef); err != nil {
		t.Fatalf("complete: %v", err)
	}
	return operation, claim.Job().ID
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

// TestCollectionEmbeddingDimensions: a collection on the default model gets
// the deployment vector size, documents and queries are both cut to it, and
// a size the model cannot produce is refused when it is configured.
func TestCollectionEmbeddingDimensions(t *testing.T) {
	f := newFixture(t)
	f.svc.SetDefaultEmbeddingDimensions(2) // the fake model returns 3 dimensions
	ctx := context.Background()
	c := f.collection(t)
	if c.EmbeddingModel != "embed-test" || c.EmbeddingDimensions != 2 {
		t.Fatalf("default collection = %s/%d, want embed-test/2", c.EmbeddingModel, c.EmbeddingDimensions)
	}
	other, err := f.svc.CreateCollection(ctx, f.principal, rag.CreateCollectionInput{Name: "other", EmbeddingModel: "other-embed"})
	if err != nil || other.EmbeddingDimensions != 0 {
		t.Fatalf("other model = %d dims, %v; want full size", other.EmbeddingDimensions, err)
	}
	tiny, wide := 8, 64
	if _, err := f.svc.CreateCollection(ctx, f.principal, rag.CreateCollectionInput{Name: "tiny", EmbeddingDimensions: &tiny}); !errors.Is(err, rag.ErrInvalidInput) {
		t.Fatalf("8 dimensions = %v, want ErrInvalidInput", err)
	}
	if _, err := f.svc.CreateCollection(ctx, f.principal, rag.CreateCollectionInput{Name: "wide", EmbeddingDimensions: &wide}); !errors.Is(err, rag.ErrInvalidInput) {
		t.Fatalf("64 dimensions from a 3-dimension model = %v, want ErrInvalidInput", err)
	}

	if _, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "note.txt", MimeType: "text/plain", Bytes: []byte("enclava vector size"),
	}); err != nil {
		t.Fatalf("upload document: %v", err)
	}
	f.runNextJob(t)
	points := f.vector.points[c.QdrantCollection]
	if len(points) == 0 {
		t.Fatal("no points indexed")
	}
	for _, pt := range points {
		if len(pt.Vector) != 2 || math.Abs(pt.Vector[0]*pt.Vector[0]+pt.Vector[1]*pt.Vector[1]-1) > 1e-9 {
			t.Fatalf("indexed vector %v, want a unit-length 2-vector", pt.Vector)
		}
	}
	if _, err := f.svc.Search(ctx, f.principal, rag.SearchInput{CollectionID: c.ID, Query: "enclava", Mode: rag.QuerySemantic}); err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(f.vector.lastQuery) != 2 {
		t.Fatalf("query vector %v, want 2 dimensions", f.vector.lastQuery)
	}
	if _, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{EmbeddingDimensions: &wide}); !errors.Is(err, rag.ErrInvalidInput) {
		t.Fatalf("resize past the model = %v, want ErrInvalidInput", err)
	}
	if got, _ := f.svc.GetCollection(ctx, f.principal, c.ID); got.EmbeddingDimensions != 2 || got.QdrantCollection != c.QdrantCollection {
		t.Fatalf("refused resize changed the collection: %d dims in %s", got.EmbeddingDimensions, got.QdrantCollection)
	}
}

// TestCollectionReembedsInBackground: switching model and size re-embeds
// every document into a new index while search keeps reading each document
// from the index it is published in; the old index is dropped once the last
// document has moved, even with an abandoned attempt still bound to it.
func TestCollectionReembedsInBackground(t *testing.T) {
	f := newFixture(t)
	addEmbeddingEndpoint(t, f.db, f.tenant, "ep-embed-wide", "embed-wide")
	ctx := context.Background()
	c := f.collection(t)
	var versionID string
	for _, name := range []string{"a.txt", "b.txt"} {
		upload, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
			FileName: name, MimeType: "text/plain", Bytes: []byte("enclava background " + name),
		})
		if err != nil {
			t.Fatalf("upload %s: %v", name, err)
		}
		versionID = upload.Document.ObjectVersionID
		f.runNextJob(t)
	}
	oldIndex := c.QdrantCollection
	// A failed attempt older than the published generation, bound to the old
	// index — what an import that died at upsert leaves behind.
	if _, err := f.db.SQL.ExecContext(ctx, `
		INSERT INTO index_generations (tenant_id, id, collection_id, object_version_id, generation, state,
		                               qdrant_collection, embedding_model, created_at)
		VALUES ($1, 'idxgen_abandoned', $2, $3, 0, 'staging', $4, 'embed-test', now() - interval '1 hour')`,
		f.tenant, c.ID, versionID, oldIndex); err != nil {
		t.Fatalf("seed abandoned attempt: %v", err)
	}
	search := func() map[string]bool {
		t.Helper()
		f.vector.queried = nil
		results, err := f.svc.Search(ctx, f.principal, rag.SearchInput{CollectionID: c.ID, Query: "enclava background", Mode: rag.QuerySemantic, Limit: 10})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		found := map[string]bool{}
		for _, r := range results {
			found[r.DocumentID] = true
		}
		return found
	}

	dims, model := 32, "embed-wide"
	updated, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{EmbeddingModel: &model, EmbeddingDimensions: &dims})
	if err != nil {
		t.Fatalf("switch model: %v", err)
	}
	newIndex := updated.QdrantCollection
	if newIndex == oldIndex || updated.EmbeddingModel != model || updated.EmbeddingDimensions != dims {
		t.Fatalf("switched collection = %s %s/%d", newIndex, updated.EmbeddingModel, updated.EmbeddingDimensions)
	}
	if found := search(); len(found) != 2 || len(f.vector.queried) != 1 || f.vector.queried[0] != oldIndex {
		t.Fatalf("before re-embedding: found %v via %v, want both docs from %s", found, f.vector.queried, oldIndex)
	}

	f.runNextJob(t) // one document moves to the new index
	if found := search(); len(found) != 2 || len(f.vector.queried) != 2 {
		t.Fatalf("mid re-embedding: found %v via %v, want both docs from both indexes", found, f.vector.queried)
	}
	if _, kept := f.vector.points[oldIndex]; !kept {
		t.Fatal("old index dropped while a document still reads it")
	}

	f.runNextJob(t) // the last document moves
	if found := search(); len(found) != 2 || len(f.vector.queried) != 1 || f.vector.queried[0] != newIndex || len(f.vector.lastQuery) != dims {
		t.Fatalf("after re-embedding: found %v via %v (query %d dims)", found, f.vector.queried, len(f.vector.lastQuery))
	}
	for _, pt := range f.vector.points[newIndex] {
		if len(pt.Vector) != dims {
			t.Fatalf("re-embedded vector has %d dimensions, want %d", len(pt.Vector), dims)
		}
	}
	if _, kept := f.vector.points[oldIndex]; kept {
		t.Fatal("old index kept after every document moved")
	}
	if got, _ := f.svc.GetCollection(ctx, f.principal, c.ID); len(got.RetiredQdrantCollections) != 0 {
		t.Fatalf("retired indexes still listed: %v", got.RetiredQdrantCollections)
	}
}

// TestCollectionEmbedsDocumentTitles: chunks embed behind their document's
// name unless the collection turns it off, which re-embeds in place.
func TestCollectionEmbedsDocumentTitles(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)
	if !c.EmbedTitles {
		t.Fatal("a new collection does not embed document names")
	}
	text := "enclava titled chunk"
	if _, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "note.txt", MimeType: "text/plain", Bytes: []byte(text),
	}); err != nil {
		t.Fatalf("upload document: %v", err)
	}
	f.runNextJob(t)
	lastInput := func() string {
		t.Helper()
		batch := f.embedding.inputs[len(f.embedding.inputs)-1]
		if len(batch) != 1 {
			t.Fatalf("embedding batch = %v, want one chunk", batch)
		}
		return batch[0]
	}
	if got, want := lastInput(), "note.txt\n"+text; got != want {
		t.Fatalf("embedded input %q, want %q", got, want)
	}
	off := false
	updated, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{EmbedTitles: &off})
	if err != nil || updated.EmbedTitles || updated.QdrantCollection != c.QdrantCollection {
		t.Fatalf("turn names off = %+v, %v; want same index without names", updated, err)
	}
	f.runNextJob(t)
	if got, want := lastInput(), text; got != want {
		t.Fatalf("embedded input %q after turning names off, want %q", got, want)
	}
	points := f.vector.points[c.QdrantCollection]
	if len(points) != 1 || points[0].Payload["search_text"] != "note.txt\n"+text {
		t.Fatalf("search text without title embeddings = %+v", points)
	}
}

func TestCollectionPrefixesAndDurableReembedding(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c, err := f.svc.CreateCollection(ctx, f.principal, rag.CreateCollectionInput{
		Name: "prefixes", EmbeddingModel: "nomic-embed-text", ACL: map[string]any{"public": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.QueryPrefix != "search_query: " || c.PassagePrefix != "search_document: " {
		t.Fatalf("default prefixes = %q, %q", c.QueryPrefix, c.PassagePrefix)
	}
	empty := ""
	explicit, err := f.svc.CreateCollection(ctx, f.principal, rag.CreateCollectionInput{
		Name: "explicit", EmbeddingModel: "nomic-embed-text", QueryPrefix: &empty, PassagePrefix: &empty,
	})
	if err != nil || explicit.QueryPrefix != "" || explicit.PassagePrefix != "" {
		t.Fatalf("explicit empty prefixes = %+v, %v", explicit, err)
	}

	addEmbeddingEndpoint(t, f.db, f.tenant, "ep-nomic", "nomic-embed-text")
	if _, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "note.txt", MimeType: "text/plain", Bytes: []byte("enclava prefix content"),
	}); err != nil {
		t.Fatal(err)
	}
	f.runNextJob(t)
	query, passage := "question: ", "document: "
	updated, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{QueryPrefix: &query, PassagePrefix: &passage})
	if err != nil {
		t.Fatal(err)
	}
	if updated.QueryPrefix != query || updated.PassagePrefix != passage || updated.QdrantCollection == c.QdrantCollection {
		t.Fatalf("updated prefixes/index = %+v", updated)
	}
	pending, total, err := f.svc.ReembedProgress(ctx, f.principal, c.ID)
	if err != nil || pending != 1 || total != 1 {
		t.Fatalf("progress before scan = %d/%d, %v", pending, total, err)
	}
	if _, err := f.svc.Search(ctx, f.principal, rag.SearchInput{CollectionID: c.ID, Query: "enclava", Mode: rag.QuerySemantic}); err != nil {
		t.Fatal(err)
	}
	oldQuery := f.embedding.inputs[len(f.embedding.inputs)-1]
	if len(oldQuery) != 1 || oldQuery[0] != "search_query: enclava" {
		t.Fatalf("old generation query input = %q", oldQuery)
	}
	if op, _ := f.runOneJob(t); op != rag.OperationReembedScan {
		t.Fatalf("settings job = %q, want scan", op)
	}
	pending, total, err = f.svc.ReembedProgress(ctx, f.principal, c.ID)
	if err != nil || pending != 1 || total != 1 {
		t.Fatalf("progress after scan = %d/%d, %v", pending, total, err)
	}
	f.runNextJob(t)
	pending, total, err = f.svc.ReembedProgress(ctx, f.principal, c.ID)
	if err != nil || pending != 0 || total != 1 {
		t.Fatalf("progress after publication = %d/%d, %v", pending, total, err)
	}
	last := f.embedding.inputs[len(f.embedding.inputs)-1]
	if len(last) != 1 || last[0] != "document: note.txt\nenclava prefix content" {
		t.Fatalf("re-embedding input = %q", last)
	}
	if _, err := f.svc.Search(ctx, f.principal, rag.SearchInput{CollectionID: c.ID, Query: "enclava", Mode: rag.QuerySemantic}); err != nil {
		t.Fatal(err)
	}
	last = f.embedding.inputs[len(f.embedding.inputs)-1]
	if len(last) != 1 || last[0] != "question: enclava" {
		t.Fatalf("query embedding input = %q", last)
	}
	queryOnly := "only-query: "
	queryUpdate, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{QueryPrefix: &queryOnly})
	if err != nil || queryUpdate.QdrantCollection != updated.QdrantCollection {
		t.Fatalf("query-only update = %+v, %v", queryUpdate, err)
	}
	generations, err := f.repo.ListPublishedGenerations(ctx, f.tenant, c.ID)
	if err != nil || len(generations) != 1 || generations[0].QueryPrefix != queryOnly || generations[0].Generation != 2 {
		t.Fatalf("query-only generation = %+v, %v", generations, err)
	}
	if _, err := f.svc.Search(ctx, f.principal, rag.SearchInput{CollectionID: c.ID, Query: "enclava", Mode: rag.QuerySemantic}); err != nil {
		t.Fatal(err)
	}
	last = f.embedding.inputs[len(f.embedding.inputs)-1]
	if len(last) != 1 || last[0] != "only-query: enclava" {
		t.Fatalf("query-only embedding input = %q", last)
	}
}

func TestUpdateCollectionSkipsScanWithoutPendingWork(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		svc  *rag.Service
	}{
		{"configured jobs", f.svc},
		{"absent jobs", rag.NewService(f.repo, f.vector, f.executor, "embed-test", nil, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := tc.svc.CreateCollection(ctx, f.principal, rag.CreateCollectionInput{
				Name: "idle " + tc.name, ACL: map[string]any{"public": true},
			})
			if err != nil {
				t.Fatal(err)
			}
			renamed := "renamed " + tc.name
			updated, err := tc.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{Name: &renamed})
			if err != nil || updated.Name != renamed {
				t.Fatalf("update without pending work = %+v, %v", updated, err)
			}
			if claim, err := f.jobs.Lease(ctx, "test-worker", time.Minute); err != nil || claim != nil {
				t.Fatalf("no pending re-embed work still queued %v, %v", claim, err)
			}
		})
	}
}

func TestUpdateCollectionScanDedupesOnlyWhileQueued(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)
	if _, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "note.txt", MimeType: "text/plain", Bytes: []byte("enclava scan dedupe"),
	}); err != nil {
		t.Fatal(err)
	}
	f.runNextJob(t)

	off, on := false, true
	if _, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{EmbedTitles: &off}); err != nil {
		t.Fatal(err)
	}
	op, first := f.runOneJob(t)
	if op != rag.OperationReembedScan {
		t.Fatalf("first title toggle job = %q, want scan", op)
	}
	f.runNextJob(t) // drain the re-embed the first scan queued
	if _, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{EmbedTitles: &on}); err != nil {
		t.Fatal(err)
	}
	op, second := f.runOneJob(t)
	if op != rag.OperationReembedScan {
		t.Fatalf("second title toggle job = %q, want scan", op)
	}
	f.runNextJob(t) // drain the re-embed the second scan queued
	if _, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{EmbedTitles: &off}); err != nil {
		t.Fatal(err)
	}
	name := "save while queued"
	if _, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{Name: &name}); err != nil {
		t.Fatal(err)
	}
	op, third := f.runOneJob(t)
	if op != rag.OperationReembedScan {
		t.Fatalf("queued-scan save job = %q, want scan", op)
	}
	seen := map[string]bool{first: true, second: true, third: true}
	if first == "" || second == "" || third == "" || len(seen) != 3 {
		t.Fatalf("completed scans deduped instead of queueing new work: %v", seen)
	}
	f.runNextJob(t)
	if claim, err := f.jobs.Lease(ctx, "test-worker", time.Minute); err != nil || claim != nil {
		t.Fatalf("queued-scan saves produced a duplicate job %v, %v", claim, err)
	}
}

// TestUpdateCollectionScanRequeuesWhileRunning: a running scan already froze
// its document list, so a settings save must queue another pass — only a
// still-queued scan dedupes a new one.
func TestUpdateCollectionScanRequeuesWhileRunning(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)
	if _, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "note.txt", MimeType: "text/plain", Bytes: []byte("enclava running scan"),
	}); err != nil {
		t.Fatal(err)
	}
	f.runNextJob(t)

	off := false
	if _, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{EmbedTitles: &off}); err != nil {
		t.Fatal(err)
	}
	running, err := f.jobs.Lease(ctx, "test-worker", time.Minute)
	if err != nil || running == nil || running.Job().Operation != rag.OperationReembedScan {
		t.Fatalf("lease first scan: %v (claim %v)", err, running)
	}
	prefix := "document: "
	if _, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{PassagePrefix: &prefix}); err != nil {
		t.Fatal(err)
	}
	second, err := f.jobs.Lease(ctx, "test-worker", time.Minute)
	if err != nil || second == nil || second.Job().Operation != rag.OperationReembedScan {
		t.Fatalf("save while a scan runs must queue another scan: %v (claim %v)", err, second)
	}
	if second.Job().ID == running.Job().ID {
		t.Fatal("the running scan was re-leased instead of queueing a new one")
	}
}

func TestIndexTitleAndMetadataBoundaries(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)
	connector, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "connector-placeholder.txt", MimeType: "text/plain", Bytes: []byte("connector body"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SQL.ExecContext(ctx, `
		UPDATE source_object_versions SET provenance='{"source_kind":"slack","title":""}'::jsonb
		WHERE tenant_id=$1 AND object_id=$2 AND observed_seq=1`, f.tenant, connector.Document.ID); err != nil {
		t.Fatal(err)
	}
	f.runNextJob(t)
	last := f.embedding.inputs[len(f.embedding.inputs)-1]
	if len(last) != 1 || last[0] != "connector body" {
		t.Fatalf("empty connector title used placeholder filename: %q", last)
	}

	longName := strings.Repeat("界", 130) + ".txt"
	if _, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: longName, MimeType: "text/plain", Bytes: []byte("upload body"),
	}); err != nil {
		t.Fatal(err)
	}
	f.runNextJob(t)
	last = f.embedding.inputs[len(f.embedding.inputs)-1]
	if len(last) != 1 || last[0] != strings.Repeat("界", 120)+"\nupload body" {
		t.Fatalf("long upload title not bounded to 120 runes: %q", last)
	}

	for _, tc := range []struct {
		name     string
		value    string
		included bool
	}{
		{"below", strings.Repeat("x", 21), true}, // "k: " + value is 24 code points
		{"boundary", strings.Repeat("x", 22), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
				FileName: tc.name + ".txt", MimeType: "text/plain", Bytes: []byte("metadata body"),
				Metadata: map[string]any{"k": tc.value},
			}); err != nil {
				t.Fatal(err)
			}
			f.runNextJob(t)
			last := f.embedding.inputs[len(f.embedding.inputs)-1]
			if len(last) != 1 || strings.Contains(last[0], "\nk: "+tc.value) != tc.included {
				t.Fatalf("embedding input at metadata boundary = %q", last)
			}
			points := f.vector.points[c.QdrantCollection]
			searchText, _ := points[len(points)-1].Payload["search_text"].(string)
			if !strings.Contains(searchText, tc.value) {
				t.Fatalf("metadata absent from search_text: %q", searchText)
			}
		})
	}
}

func TestIndexMetadataSearchTextAndWarnings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)
	upload, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "note.txt", MimeType: "text/plain", Bytes: []byte("enclava body"),
		Metadata: map[string]any{"z": "last", "a": "first"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.runNextJob(t)
	last := f.embedding.inputs[len(f.embedding.inputs)-1]
	if len(last) != 1 || last[0] != "note.txt\nenclava body\na: first\nz: last" {
		t.Fatalf("embedding input = %q", last)
	}
	points := f.vector.points[c.QdrantCollection]
	if len(points) != 1 {
		t.Fatalf("points = %d, want 1", len(points))
	}
	searchText, _ := points[0].Payload["search_text"].(string)
	if !strings.Contains(searchText, "note.txt") || !strings.Contains(searchText, "enclava body") || !strings.Contains(searchText, "first") || !strings.Contains(searchText, "last") {
		t.Fatalf("search_text = %q", searchText)
	}
	if points[0].Payload["content"] != "enclava body" {
		t.Fatalf("metadata changed authoritative chunk: %v", points[0].Payload["content"])
	}
	chunks, err := f.repo.ListGenerationChunks(ctx, f.tenant, c.ID, upload.Document.ObjectVersionID, 1)
	if err != nil || len(chunks) != 1 || chunks[0].Content != "enclava body" {
		t.Fatalf("stored chunks = %+v, %v", chunks, err)
	}
	version, err := f.repo.GetObjectVersion(ctx, f.tenant, upload.Document.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.artifactRoot, f.tenant, version.ArtifactRef)); err != nil {
		t.Fatalf("withhold original bytes: %v", err)
	}

	prefix := "passage: "
	updated, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{PassagePrefix: &prefix})
	if err != nil {
		t.Fatal(err)
	}
	f.runNextJob(t)
	reused, err := f.repo.ListGenerationChunks(ctx, f.tenant, c.ID, upload.Document.ObjectVersionID, 2)
	if err != nil || len(reused) != len(chunks) || !reflect.DeepEqual(reused[0].SpanRefs, chunks[0].SpanRefs) {
		t.Fatalf("re-embedding did not reuse chunk spans: old=%+v new=%+v err=%v", chunks, reused, err)
	}
	if got := f.vector.points[updated.QdrantCollection]; len(got) != 1 || got[0].Payload["content"] != "enclava body" {
		t.Fatalf("re-embedded points = %+v", got)
	}
	if _, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "large.txt", MimeType: "text/plain", Bytes: []byte("enclava small body"),
		Metadata: map[string]any{"long": strings.Repeat("x", 100)},
	}); err != nil {
		t.Fatal(err)
	}
	f.runNextJob(t)
	last = f.embedding.inputs[len(f.embedding.inputs)-1]
	if len(last) != 1 || strings.Contains(last[0], strings.Repeat("x", 100)) {
		t.Fatalf("oversized metadata in embedding input = %q", last)
	}

	warned, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "encoding.txt", MimeType: "text/plain", Bytes: []byte{'c', 'a', 'f', 0xe9},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.runNextJob(t)
	document := f.docStatus(t, c.ID, warned.Document.ID)
	if len(document.Warnings) == 0 || !strings.Contains(strings.Join(document.Warnings, " "), "windows-1252") {
		t.Fatalf("document warnings = %q", document.Warnings)
	}
	if _, err := f.svc.ReprocessDocument(ctx, f.principal, warned.Document.ID); err != nil {
		t.Fatal(err)
	}
	f.runNextJob(t)
	if got := f.docStatus(t, c.ID, warned.Document.ID).Warnings; !reflect.DeepEqual(got, document.Warnings) {
		t.Fatalf("warnings changed after chunk reuse: before=%q after=%q", document.Warnings, got)
	}
}

func TestRetiredIndexWaitsForSoakAndReplacementPoints(t *testing.T) {
	f := newFixture(t)
	f.svc.SetIndexRetirementSoak(time.Hour)
	ctx := context.Background()
	c := f.collection(t)
	if _, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "note.txt", MimeType: "text/plain", Bytes: []byte("enclava retirement"),
	}); err != nil {
		t.Fatal(err)
	}
	f.runNextJob(t)
	prefix := "passage: "
	updated, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{PassagePrefix: &prefix})
	if err != nil {
		t.Fatal(err)
	}
	f.runNextJob(t)
	if _, exists := f.vector.points[c.QdrantCollection]; !exists {
		t.Fatal("retired index dropped during soak")
	}
	f.svc.SetIndexRetirementSoak(time.Nanosecond)
	replacement := f.vector.points[updated.QdrantCollection]
	f.vector.points[updated.QdrantCollection] = nil
	if _, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{}); err != nil {
		t.Fatal(err)
	}
	if _, exists := f.vector.points[c.QdrantCollection]; !exists {
		t.Fatal("retired index dropped without replacement points")
	}
	f.vector.points[updated.QdrantCollection] = replacement
	if _, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{}); err != nil {
		t.Fatal(err)
	}
	if _, exists := f.vector.points[c.QdrantCollection]; exists {
		t.Fatal("unused retired index kept after soak and replacement verification")
	}
}

// TestOlderAttemptNeverReplacesNewerGeneration: an attempt still embedding
// when a newer generation of its document publishes under switched settings
// is superseded instead of publishing over it, so cleanup can drop the old
// index without leaving the document unsearchable.
func TestOlderAttemptNeverReplacesNewerGeneration(t *testing.T) {
	f := newFixture(t)
	addEmbeddingEndpoint(t, f.db, f.tenant, "ep-embed-wide", "embed-wide")
	ctx := context.Background()
	c := f.collection(t)
	upload, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "a.txt", MimeType: "text/plain", Bytes: []byte("enclava fenced publication"),
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	f.runNextJob(t) // generation 1, original index
	if _, err := f.svc.ReprocessDocument(ctx, f.principal, upload.Document.ID); err != nil {
		t.Fatalf("reprocess: %v", err)
	}
	older, err := f.jobs.Lease(ctx, "slow-worker", time.Minute) // generation 2
	if err != nil || older == nil {
		t.Fatalf("lease older attempt: %v", err)
	}
	dims, model := 32, "embed-wide"
	f.embedding.midEmbed = func() {
		if _, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{EmbeddingModel: &model, EmbeddingDimensions: &dims}); err != nil {
			t.Fatalf("switch settings: %v", err)
		}
		if _, err := f.svc.ReprocessDocument(ctx, f.principal, upload.Document.ID); err != nil {
			t.Fatalf("reprocess under new settings: %v", err)
		}
		f.runNextJob(t) // generation 3 publishes first
	}
	ref, err := f.svc.JobHandler()(ctx, older, nil)
	if err != nil {
		t.Fatalf("older attempt: %v", err)
	}
	if _, err := older.Complete(ctx, ref); err != nil {
		t.Fatalf("complete older attempt: %v", err)
	}

	current, _ := f.svc.GetCollection(ctx, f.principal, c.ID)
	if _, kept := f.vector.points[c.QdrantCollection]; kept {
		t.Fatalf("original index %s kept after its last generation was superseded", c.QdrantCollection)
	}
	for _, pt := range f.vector.points[current.QdrantCollection] {
		if pt.Payload["generation"] != int64(3) {
			t.Fatalf("index holds generation %v points, want only generation 3", pt.Payload["generation"])
		}
	}
	results, err := f.svc.Search(ctx, f.principal, rag.SearchInput{CollectionID: c.ID, Query: "enclava fenced", Mode: rag.QuerySemantic})
	if err != nil || len(results) == 0 || results[0].Generation != 3 {
		t.Fatalf("search after the race = %+v, %v; want generation 3", results, err)
	}
}

// TestOlderAttemptCannotSupersedeNewer: on an unchanged index, an older
// generation finishing after a newer one published is refused and its
// points dropped; the newer generation stays the live one.
func TestOlderAttemptCannotSupersedeNewer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)
	upload, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "a.txt", MimeType: "text/plain", Bytes: []byte("enclava publication order"),
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	f.runNextJob(t) // generation 1
	if _, err := f.svc.ReprocessDocument(ctx, f.principal, upload.Document.ID); err != nil {
		t.Fatalf("reprocess: %v", err)
	}
	older, err := f.jobs.Lease(ctx, "slow-worker", time.Minute) // generation 2
	if err != nil || older == nil {
		t.Fatalf("lease older attempt: %v", err)
	}
	f.embedding.midEmbed = func() {
		if _, err := f.svc.ReprocessDocument(ctx, f.principal, upload.Document.ID); err != nil {
			t.Fatalf("reprocess again: %v", err)
		}
		f.runNextJob(t) // generation 3 publishes first
	}
	ref, err := f.svc.JobHandler()(ctx, older, nil)
	if err != nil {
		t.Fatalf("older attempt: %v", err)
	}
	if _, err := older.Complete(ctx, ref); err != nil {
		t.Fatalf("complete older attempt: %v", err)
	}
	for _, pt := range f.vector.points[c.QdrantCollection] {
		if pt.Payload["generation"] != int64(3) {
			t.Fatalf("index holds generation %v points, want only generation 3", pt.Payload["generation"])
		}
	}
	results, err := f.svc.Search(ctx, f.principal, rag.SearchInput{CollectionID: c.ID, Query: "enclava publication", Mode: rag.QuerySemantic})
	if err != nil || len(results) == 0 || results[0].Generation != 3 {
		t.Fatalf("search = %+v, %v; want generation 3 live", results, err)
	}
}

// TestSettingsRetryResumesReembedding: a settings change whose re-embedding
// never got queued (interrupted request) is finished by saving the same
// settings again, and switching back to earlier settings never reuses an
// index name that cleanup may still be dropping.
func TestSettingsRetryResumesReembedding(t *testing.T) {
	f := newFixture(t)
	addEmbeddingEndpoint(t, f.db, f.tenant, "ep-embed-wide", "embed-wide")
	ctx := context.Background()
	c := f.collection(t)
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
			FileName: name, MimeType: "text/plain", Bytes: []byte("enclava resume " + name),
		}); err != nil {
			t.Fatalf("upload %s: %v", name, err)
		}
		f.runNextJob(t)
	}
	// The interrupted change: new settings committed, nothing enqueued.
	interrupted := c
	interrupted.EmbeddingModel, interrupted.EmbeddingDimensions = "embed-wide", 32
	interrupted.QdrantCollection = c.QdrantCollection + "_interrupted"
	if err := f.repo.UpdateCollection(ctx, interrupted); err != nil {
		t.Fatalf("commit settings: %v", err)
	}
	dims, model := 32, "embed-wide"
	if _, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{EmbeddingModel: &model, EmbeddingDimensions: &dims}); err != nil {
		t.Fatalf("retry settings: %v", err)
	}
	for range 3 {
		f.runNextJob(t)
	}
	if claim, err := f.jobs.Lease(ctx, "test-worker", time.Minute); err != nil || claim != nil {
		t.Fatalf("unexpected extra job after resuming: %v %v", claim, err)
	}
	if got := len(f.vector.points[interrupted.QdrantCollection]); got != 3 {
		t.Fatalf("new index holds %d points, want all 3 documents", got)
	}
	if _, kept := f.vector.points[c.QdrantCollection]; kept {
		t.Fatal("old index kept after every document moved")
	}

	original, zero := "embed-test", 0
	back, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{EmbeddingModel: &original, EmbeddingDimensions: &zero})
	if err != nil {
		t.Fatalf("switch back: %v", err)
	}
	if back.QdrantCollection == c.QdrantCollection || back.QdrantCollection == interrupted.QdrantCollection {
		t.Fatalf("switching back reused index %s", back.QdrantCollection)
	}
}

// TestAttemptUnderStaleSettingsRequeues: a document that publishes under
// settings switched while it was embedding (its old index still serving
// another document) is queued again under the current ones, so no document
// stays on a retired index.
func TestAttemptUnderStaleSettingsRequeues(t *testing.T) {
	f := newFixture(t)
	addEmbeddingEndpoint(t, f.db, f.tenant, "ep-embed-wide", "embed-wide")
	ctx := context.Background()
	c := f.collection(t)
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
			FileName: name, MimeType: "text/plain", Bytes: []byte("enclava stale settings " + name),
		}); err != nil {
			t.Fatalf("upload %s: %v", name, err)
		}
		if name == "a.txt" {
			f.runNextJob(t) // a.txt published, keeping the old index in use
		}
	}
	dims, model := 32, "embed-wide"
	f.embedding.midEmbed = func() {
		if _, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{EmbeddingModel: &model, EmbeddingDimensions: &dims}); err != nil {
			t.Fatalf("switch settings: %v", err)
		}
	}
	f.runNextJob(t) // b.txt publishes under the old settings, then requeues
	f.runNextJob(t) // a.txt, queued by the switch
	f.runNextJob(t) // b.txt under the new settings
	current, _ := f.svc.GetCollection(ctx, f.principal, c.ID)
	points := f.vector.points[current.QdrantCollection]
	if len(points) != 2 || len(points[0].Vector) != dims || len(points[1].Vector) != dims {
		t.Fatalf("current index points = %v, want both documents at %d dimensions", points, dims)
	}
	if _, kept := f.vector.points[c.QdrantCollection]; kept {
		t.Fatal("old index kept after every document moved")
	}
}

// staleRequeueRepo injects failures into the stale-settings requeue an
// attempt performs after publishing. failRead makes the collection re-read
// fail once the document's generation is published — the earlier mid-attempt
// re-read still runs before publishing and passes — failDenied makes it
// report the collection gone, and failStage makes staging any generation
// newer than the published one (the successor's) fail. armed only turns on
// inside midEmbed, so the upload's own staging and the mid-attempt re-read
// are unaffected.
type staleRequeueRepo struct {
	rag.Repository
	armed        bool
	failRead     bool
	failDenied   bool
	failStage    bool
	collectionID string
	versionID    string
	published    int64
}

func (r *staleRequeueRepo) GetCollection(ctx context.Context, tenantID, id string) (rag.Collection, error) {
	if r.armed && (r.failRead || r.failDenied) && id == r.collectionID {
		published, err := r.PublishedGenerationForVersion(ctx, tenantID, r.collectionID, r.versionID, r.published)
		if err == nil && published {
			if r.failDenied {
				return rag.Collection{}, source.ErrDenied
			}
			return rag.Collection{}, errors.New("injected collection read failure")
		}
	}
	return r.Repository.GetCollection(ctx, tenantID, id)
}

func (r *staleRequeueRepo) ReplaceVersionChunksAndStage(ctx context.Context, tenantID, collectionID, objectID, versionID string, seq, generation int64, profileVersionID string, chunks []rag.DocumentChunk) error {
	if r.armed && r.failStage && generation > r.published {
		return errors.New("injected stage failure")
	}
	return r.Repository.ReplaceVersionChunksAndStage(ctx, tenantID, collectionID, objectID, versionID, seq, generation, profileVersionID, chunks)
}

// TestStaleSettingsRequeueFailureFailsAttempt: when a settings change lands
// mid-embed, the attempt re-queues the document under the current settings
// after publishing. A failed collection re-read or successor enqueue must
// fail the attempt — reporting success would drop the re-embed the settings
// scan already skipped while the document was in flight — and the retried
// attempt must skip the re-embed of its published generation and finish the
// requeue. A deleted collection needs no successor: the attempt completes.
func TestStaleSettingsRequeueFailureFailsAttempt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failRead   bool
		failDenied bool
		failStage  bool
		wantErr    bool
	}{
		{"collection re-read fails", true, false, false, true},
		{"successor staging fails", false, false, true, true},
		{"collection deleted", false, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			c := f.collection(t)
			upload, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
				FileName: "a.txt", MimeType: "text/plain", Bytes: []byte("enclava stale requeue " + tc.name),
			})
			if err != nil {
				t.Fatal(err)
			}
			repo := &staleRequeueRepo{
				Repository:   f.repo,
				failRead:     tc.failRead,
				failDenied:   tc.failDenied,
				failStage:    tc.failStage,
				collectionID: c.ID,
				versionID:    upload.Document.ObjectVersionID,
				published:    1,
			}
			svc := rag.NewService(repo, f.vector, f.executor, "embed-test", f.store, f.jobs)
			off := false
			f.embedding.midEmbed = func() {
				if _, err := svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{EmbedTitles: &off}); err != nil {
					t.Fatalf("switch settings: %v", err)
				}
				repo.armed = true
			}
			claim, err := f.jobs.Lease(ctx, "test-worker", time.Minute)
			if err != nil || claim == nil {
				t.Fatalf("lease: %v", err)
			}
			ref, err := svc.JobHandler()(ctx, claim, nil)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("attempt on a deleted collection = %v, want success", err)
				}
				if _, err := claim.Complete(ctx, ref); err != nil {
					t.Fatalf("complete: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("stale-settings requeue failure was acknowledged as success")
			}
			if jobs.FailureClassFor(err) != jobs.FailureRetryable {
				t.Fatalf("requeue failure = %v, want a retryable error so the job is retried", err)
			}
			if err := claim.Fail(ctx, err, jobs.FailureRetryable); err != nil {
				t.Fatalf("fail attempt: %v", err)
			}
			// The retried attempt finds its generation published: it skips the
			// billed re-embed and finishes the successor enqueue.
			repo.armed = false
			embeds := f.embedding.calls
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				if claim, err = f.jobs.Lease(ctx, "test-worker", time.Minute); err != nil || claim != nil || time.Now().After(deadline) {
					break
				}
			}
			if err != nil || claim == nil {
				t.Fatalf("lease retry: %v", err)
			}
			ref, err = svc.JobHandler()(ctx, claim, nil)
			if err != nil {
				t.Fatalf("retried attempt: %v", err)
			}
			if f.embedding.calls != embeds {
				t.Fatal("retried attempt re-embedded the published generation")
			}
			if _, err := claim.Complete(ctx, ref); err != nil {
				t.Fatalf("complete retry: %v", err)
			}
			f.runNextJob(t) // drain the settings scan, then run the successor embed
			pending, total, err := f.svc.ReembedProgress(ctx, f.principal, c.ID)
			if err != nil || pending != 0 || total != 1 {
				t.Fatalf("progress after retry = %d/%d, %v", pending, total, err)
			}
		})
	}
}

// TestAttemptRetriesWhenIndexDropped: an attempt whose index was retired and
// dropped while it embedded retries under the current settings instead of
// recreating the dropped index.
func TestAttemptRetriesWhenIndexDropped(t *testing.T) {
	f := newFixture(t)
	addEmbeddingEndpoint(t, f.db, f.tenant, "ep-embed-wide", "embed-wide")
	ctx := context.Background()
	c := f.collection(t)
	if _, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "a.txt", MimeType: "text/plain", Bytes: []byte("enclava dropped index"),
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	dims, model := 32, "embed-wide"
	f.embedding.midEmbed = func() {
		// Nothing is published yet, so the switch drops the old index at once.
		if _, err := f.svc.UpdateCollection(ctx, f.principal, c.ID, rag.UpdateCollectionInput{EmbeddingModel: &model, EmbeddingDimensions: &dims}); err != nil {
			t.Fatalf("switch settings: %v", err)
		}
	}
	claim, err := f.jobs.Lease(ctx, "test-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("lease: %v", err)
	}
	_, err = f.svc.JobHandler()(ctx, claim, nil)
	if jobs.FailureClassFor(err) != jobs.FailureRetryable {
		t.Fatalf("attempt on a dropped index = %v, want a retryable error", err)
	}
	if _, recreated := f.vector.points[c.QdrantCollection]; recreated {
		t.Fatal("dropped index was recreated")
	}
	if err := claim.Fail(ctx, err, jobs.FailureRetryable); err != nil {
		t.Fatalf("fail attempt: %v", err)
	}
	// The retried attempt becomes leasable after the runner's short backoff;
	// nothing is published, so no re-embed scan was queued.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if claim, err = f.jobs.Lease(ctx, "test-worker", time.Minute); err != nil || claim != nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil || claim == nil {
		t.Fatalf("lease retry: %v", err)
	}
	ref, err := f.svc.JobHandler()(ctx, claim, nil)
	if err != nil {
		t.Fatalf("retried attempt: %v", err)
	}
	if _, err := claim.Complete(ctx, ref); err != nil {
		t.Fatalf("complete retry: %v", err)
	}
	current, _ := f.svc.GetCollection(ctx, f.principal, c.ID)
	if points := f.vector.points[current.QdrantCollection]; len(points) != 1 || len(points[0].Vector) != dims {
		t.Fatalf("current index points = %v, want the document at %d dimensions", points, dims)
	}
}

func TestAttemptRetriesTransientVectorFailure(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)
	if _, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "retry.txt", MimeType: "text/plain", Bytes: []byte("transient vector failure"),
	}); err != nil {
		t.Fatal(err)
	}
	f.vector.ensureErr = fmt.Errorf("qdrant unavailable: %w", rag.ErrVectorStoreUnavailable)
	claim, err := f.jobs.Lease(ctx, "test-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("lease: %v", err)
	}
	_, err = f.svc.JobHandler()(ctx, claim, nil)
	if jobs.FailureClassFor(err) != jobs.FailureRetryable {
		t.Fatalf("transient vector failure = %v, want retryable", err)
	}
	if err := claim.Fail(ctx, err, jobs.FailureRetryable); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if claim, err = f.jobs.Lease(ctx, "test-worker", time.Minute); err != nil || claim != nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil || claim == nil {
		t.Fatalf("lease retry: %v", err)
	}
	ref, err := f.svc.JobHandler()(ctx, claim, nil)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, err := claim.Complete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	results, err := f.svc.Search(ctx, f.principal, rag.SearchInput{CollectionID: c.ID, Query: "transient vector", Mode: rag.QueryLexical})
	if err != nil || len(results) != 1 {
		t.Fatalf("search after retry = %#v, %v", results, err)
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

// enumerationRepo fails any whole-collection generation listing: the token
// prefilter path must never enumerate documents to search.
type enumerationRepo struct {
	*postgres.RAGRepo
	listed bool
}

func (r *enumerationRepo) ListPublishedGenerations(ctx context.Context, tenantID, collectionID string) ([]rag.IndexGeneration, error) {
	r.listed = true
	return nil, errors.New("generation enumeration must not run during search")
}

func (r *enumerationRepo) ListCollectionItems(ctx context.Context, tenantID, collectionID string) ([]string, error) {
	r.listed = true
	return nil, errors.New("collection item enumeration must not run during search or evidence reads")
}

func (r *enumerationRepo) ListPublishedGenerationsBounded(ctx context.Context, tenantID, collectionID string, limit int) ([]rag.IndexGeneration, error) {
	r.listed = true
	return nil, errors.New("generation enumeration must not run during search")
}

// TestSearchBeyondDocumentBound: a collection past the old 2,000-document
// enumeration bound stays searchable — bindings come from one aggregate
// read and hits authorize individually, never by listing the corpus.
func TestSearchBeyondDocumentBound(t *testing.T) {
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "rag-bounds")
	user := testdb.SeedUser(t, db, tenant, "rag-bounds@example.test", nil)
	repo := &enumerationRepo{RAGRepo: postgres.NewRAGRepo(db)}
	vector := newFakeVectorStore()
	svc := rag.NewService(repo, vector, nil, "embed-test", nil, nil)
	p := auth.Principal{TenantID: tenant, ActorID: user.ID, UserID: user.ID, ActorKind: auth.ActorKindUser,
		Scopes: map[string]struct{}{"rag": {}, "rag.manage": {}}}
	f := &fixture{db: db, svc: svc, repo: repo.RAGRepo, vector: vector, tenant: tenant, principal: p}
	ctx := context.Background()
	collection, err := svc.CreateCollection(ctx, p, rag.CreateCollectionInput{Name: "wide", ACL: map[string]any{"public": true}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		f.stageSearchDoc(t, collection, fmt.Sprintf("obj_w_%d", i), fmt.Sprintf("bounded work %d", i), `{}`)
	}
	// 2,100 published audience-* generations with no index points: they
	// define the aggregate binding but can never reach the post-check.
	for i, stmt := range []string{
		`INSERT INTO source_objects (tenant_id, id, kind, native_id, authorization_mode, audience, observed_seq)
		 SELECT $1, 'bulk_' || g, 'document', 'bulk_' || g, 'audience', '["*"]'::jsonb, 1
		 FROM generate_series(1, 2100) g`,
		`INSERT INTO source_object_versions (tenant_id, id, object_id, observed_seq, artifact_ref, content_sha256, byte_size, provenance)
		 SELECT $1, id || '#1', id, 1, 'art_' || id, 'sha_' || id, 10, '{}'::jsonb
		 FROM source_objects WHERE tenant_id=$1 AND id LIKE 'bulk_%'`,
		`INSERT INTO collection_items (tenant_id, id, collection_id, object_id)
		 SELECT $1, 'ci_' || id, $2, id
		 FROM source_objects WHERE tenant_id=$1 AND id LIKE 'bulk_%'`,
		`INSERT INTO index_generations (tenant_id, id, collection_id, object_version_id, generation, state, qdrant_collection, embedding_model, access_projected)
		 SELECT $1, 'g_' || id, $2, id || '#1', 1, 'published', $3, 'embed-test', TRUE
		 FROM source_objects WHERE tenant_id=$1 AND id LIKE 'bulk_%'`,
	} {
		args := []any{tenant, collection.ID, collection.QdrantCollection}
		if _, err := db.SQL.ExecContext(ctx, stmt, args[:[]int{1, 1, 2, 3}[i]]...); err != nil {
			t.Fatalf("bulk fixture: %v", err)
		}
	}
	results, err := svc.Search(ctx, p, rag.SearchInput{CollectionID: collection.ID, Query: "bounded work", Mode: rag.QueryLexical, Limit: 5})
	if err != nil {
		t.Fatalf("search over 2,103 documents = %v, want success", err)
	}
	if repo.listed {
		t.Fatal("search enumerated the collection's generations")
	}
	if len(results) != 3 {
		t.Fatalf("results=%d, want the 3 staged documents only: %#v", len(results), results)
	}
	contextResult, err := svc.BuildContext(ctx, p, results[:1], 100)
	if err != nil || !strings.Contains(contextResult.Text, "bounded work") {
		t.Fatalf("context = %#v, err=%v", contextResult, err)
	}
	text, err := svc.ReadEvidence(ctx, p, results[0].EvidenceID)
	if err != nil || !strings.Contains(text.Content, "bounded work") {
		t.Fatalf("evidence text = %#v, err=%v", text, err)
	}
	resolved, err := svc.ResolveEvidence(ctx, p, []string{results[0].EvidenceID})
	if err != nil || len(resolved) != 1 || resolved[0].Status != "current" || resolved[0].CurrentGeneration != results[0].Generation {
		t.Fatalf("evidence resolution = %#v, err=%v", resolved, err)
	}
	if repo.listed {
		t.Fatal("context or evidence enumerated generations")
	}
	if _, err := svc.SearchReadable(ctx, p, "bounded work", rag.QueryLexical, nil, 5); err != nil {
		t.Fatalf("readable search over the bound = %v, want success", err)
	}
	if repo.listed {
		t.Fatal("readable search enumerated generations")
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

// TestAccessBackfillProjectsLegacyPoints: staged points written before the
// access projection pass the prefilter via the is_empty clauses; the
// backfill then writes acl+published, marks the row projected and is a
// no-op when run again.
func TestAccessBackfillProjectsLegacyPoints(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c, err := f.svc.CreateCollection(ctx, f.principal, rag.CreateCollectionInput{Name: "legacy", ACL: map[string]any{"public": true}})
	if err != nil {
		t.Fatal(err)
	}
	f.stageSearchDoc(t, c, "obj_legacy", "legacy corpus", `{}`)
	var projectedBefore int
	if err := f.db.SQL.QueryRowContext(ctx,
		`SELECT count(*) FROM index_generations WHERE tenant_id=$1 AND state='published' AND access_projected`, f.tenant).Scan(&projectedBefore); err != nil {
		t.Fatal(err)
	}
	if projectedBefore != 0 {
		t.Fatalf("staged generation must start unprojected, got %d", projectedBefore)
	}
	// Legacy points still answer through the is_empty clauses pre-backfill.
	pre, err := f.svc.Search(ctx, f.principal, rag.SearchInput{CollectionID: c.ID, Query: "legacy corpus", Mode: rag.QueryLexical, Limit: 5})
	if err != nil || len(pre) != 1 {
		t.Fatalf("unprojected point must still search: %#v err=%v", pre, err)
	}

	// Enqueue + run the durable backfill once.
	system := auth.Principal{TenantID: f.tenant, ActorID: "system", ActorKind: auth.ActorKindServiceAccount}
	if _, _, err := f.jobs.Enqueue(ctx, system, jobs.EnqueueRequest{
		TenantID: f.tenant, Operation: rag.OperationAccessBackfill, IdempotencyKey: "bf-1",
		InputRef: f.tenant, InputHash: "backfill", Policy: map[string]any{"tenant_id": f.tenant},
	}); err != nil {
		t.Fatal(err)
	}
	claim, err := f.jobs.Lease(ctx, "test-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("lease backfill: %v", err)
	}
	result, err := f.svc.AccessBackfillJobHandler()(ctx, claim, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claim.Complete(ctx, result); err != nil {
		t.Fatal(err)
	}

	// Points now carry the projected allow tokens and published=true.
	var payload map[string]any
	for _, point := range f.vector.points[c.QdrantCollection] {
		payload = point.Payload
	}
	if payload == nil || payload["published"] != true {
		t.Fatalf("backfill must set published: %#v", payload)
	}
	acl, ok := payload["acl"].([]string)
	if !ok || len(acl) != 2 || !slices.Contains(acl, "actor:"+f.principal.ActorID) || !slices.Contains(acl, "tenant") {
		t.Fatalf("backfilled acl = %#v, want actor+tenant tokens", payload["acl"])
	}
	var projectedAfter int
	if err := f.db.SQL.QueryRowContext(ctx,
		`SELECT count(*) FROM index_generations WHERE tenant_id=$1 AND state='published' AND access_projected`, f.tenant).Scan(&projectedAfter); err != nil {
		t.Fatal(err)
	}
	if projectedAfter != 1 {
		t.Fatalf("projected rows = %d, want 1", projectedAfter)
	}
	post, err := f.svc.Search(ctx, f.principal, rag.SearchInput{CollectionID: c.ID, Query: "legacy corpus", Mode: rag.QueryLexical, Limit: 5})
	if err != nil || len(post) != 1 {
		t.Fatalf("projected point must still search: %#v err=%v", post, err)
	}

	// Idempotent: a second run finds no unprojected backlog.
	if _, _, err := f.jobs.Enqueue(ctx, system, jobs.EnqueueRequest{
		TenantID: f.tenant, Operation: rag.OperationAccessBackfill, IdempotencyKey: "bf-2",
		InputRef: f.tenant, InputHash: "backfill", Policy: map[string]any{"tenant_id": f.tenant},
	}); err != nil {
		t.Fatal(err)
	}
	claim2, err := f.jobs.Lease(ctx, "test-worker", time.Minute)
	if err != nil || claim2 == nil {
		t.Fatalf("lease second backfill: %v", err)
	}
	result2, err := f.svc.AccessBackfillJobHandler()(ctx, claim2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result2 != "projected=0" {
		t.Fatalf("second run = %q, want projected=0", result2)
	}
	if _, err := claim2.Complete(ctx, result2); err != nil {
		t.Fatal(err)
	}

	// Startup enqueue dedupes against live jobs for the same tenant.
	if err := f.svc.EnqueueAccessBackfills(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationFailureAfterPayloadPreservesOldGeneration(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)
	upload, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "a.txt", MimeType: "text/plain", Bytes: []byte("publication remains visible"),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.runNextJob(t)
	if _, err := f.svc.ReprocessDocument(ctx, f.principal, upload.Document.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := f.jobs.Lease(ctx, "worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("lease: %v", err)
	}
	f.vector.onPayload = func() {
		if _, err := f.db.SQL.ExecContext(ctx, `UPDATE jobs SET lease_expires_at=now()-interval '1 second' WHERE tenant_id=$1 AND id=$2`, f.tenant, claim.JobID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.svc.JobHandler()(ctx, claim, nil); err == nil {
		t.Fatal("expired publication fence succeeded")
	}
	f.vector.onPayload = nil
	results, err := f.svc.Search(ctx, f.principal, rag.SearchInput{CollectionID: c.ID, Query: "publication", Mode: rag.QueryLexical})
	if err != nil || len(results) != 1 || results[0].Generation != 1 {
		t.Fatalf("search after failed publication = %#v, %v", results, err)
	}
}

func TestAccessBackfillLoopResumesInterruptedJob(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)
	f.stageSearchDoc(t, c, "obj_interrupted", "interrupted backfill", `{}`)
	if err := f.svc.EnqueueAccessBackfills(ctx); err != nil {
		t.Fatal(err)
	}
	claim, err := f.jobs.Lease(ctx, "crashed", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("lease: %v", err)
	}
	if err := claim.MarkDispatched(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SQL.ExecContext(ctx, `UPDATE jobs SET lease_expires_at=now()-interval '1 second' WHERE tenant_id=$1 AND id=$2`, f.tenant, claim.JobID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.jobs.Lease(ctx, "sweeper", time.Minute); err != nil {
		t.Fatal(err)
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); f.svc.RunAccessBackfillLoop(loopCtx, 10*time.Millisecond) }()
	defer func() { cancel(); <-done }()
	var retry *jobs.Claim
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		retry, err = f.jobs.Lease(ctx, "retry", time.Minute)
		if err != nil || retry != nil {
			break
		}
	}
	if err != nil || retry == nil {
		t.Fatalf("backfill was not re-enqueued: %v", err)
	}
	result, err := f.svc.AccessBackfillJobHandler()(ctx, retry, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retry.Complete(ctx, result); err != nil {
		t.Fatal(err)
	}
	if result != "projected=1" {
		t.Fatalf("backfill result = %q", result)
	}
}

func TestBackfillDoesNotOverwriteNewerProjection(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)
	f.stageSearchDoc(t, c, "obj_revision", "revision test", `{}`)
	if _, err := f.db.SQL.ExecContext(ctx, `UPDATE index_generations SET acl_revision=5 WHERE tenant_id=$1 AND collection_id=$2`, f.tenant, c.ID); err != nil {
		t.Fatal(err)
	}
	for _, point := range f.vector.points[c.QdrantCollection] {
		point.Payload["acl"] = []string{"newer"}
		point.Payload["acl_revision"] = int64(5)
		point.Payload["published"] = true
	}
	if err := f.svc.EnqueueAccessBackfills(ctx); err != nil {
		t.Fatal(err)
	}
	claim, err := f.jobs.Lease(ctx, "worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("lease: %v", err)
	}
	if _, err := f.svc.AccessBackfillJobHandler()(ctx, claim, nil); err != nil {
		t.Fatal(err)
	}
	for _, point := range f.vector.points[c.QdrantCollection] {
		if !reflect.DeepEqual(point.Payload["acl"], []string{"newer"}) || point.Payload["acl_revision"] != int64(5) {
			t.Fatalf("newer payload overwritten: %#v", point.Payload)
		}
	}
}

// TestExtractionResultImportRequiresValidationAck: a succeeded extract job
// whose stored result carries validation_errors is refused until the caller
// explicitly acknowledges them; a clean result imports without the flag.
func TestExtractionResultImportRequiresValidationAck(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)

	w, inputRef, err := f.store.Begin(ctx, artifacts.Meta{Ref: artifacts.Ref{TenantID: f.tenant, Kind: artifacts.KindOriginal}, MimeType: "text/plain"}, time.Minute, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("source body")); err != nil {
		t.Fatal(err)
	}
	if err := w.Finalize(ctx); err != nil {
		t.Fatal(err)
	}
	inputRef = w.Ref()
	if _, err := f.repo.UpsertObject(ctx, source.Object{TenantID: f.tenant, ID: "extract_src2", Kind: "upload", NativeID: "extract-native2",
		AuthorizationMode: source.AuthorizationOwner, OwnerActorID: f.principal.ActorID, ObservedSeq: 1}); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.InsertObjectVersion(ctx, f.tenant, source.ObjectVersion{ObjectID: "extract_src2", ObservedSeq: 1,
		ArtifactRef: inputRef.ID, SHA256: inputRef.SHA256, ByteSize: inputRef.ByteSize, MimeType: "text/plain", OriginalFilename: "source.txt"}); err != nil {
		t.Fatal(err)
	}

	resultDoc := []byte(`{"id":"res_needs","data":{"amount":"oops"},"validation_errors":["schema validation failed at /amount"]}`)
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
		TenantID: f.tenant, Operation: "extract.process", IdempotencyKey: "extract-ack-1",
		InputRef: inputRef.ID, Policy: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := f.jobs.Lease(ctx, "test-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatal("lease")
	}
	if _, err := claim.Complete(ctx, resultRef.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.ImportStoredResult(ctx, f.principal, rag.ImportResultInput{CollectionID: c.ID, ResultJobID: job.ID}); !errors.Is(err, rag.ErrValidationAckRequired) {
		t.Fatalf("unacknowledged import = %v, want ErrValidationAckRequired", err)
	}
	out, err := f.svc.ImportStoredResult(ctx, f.principal, rag.ImportResultInput{CollectionID: c.ID, ResultJobID: job.ID, AcknowledgeValidation: true})
	if err != nil {
		t.Fatalf("acknowledged import: %v", err)
	}
	if out.Document.SourceType != "generated" {
		t.Fatalf("imported document: %#v", out.Document)
	}
}
