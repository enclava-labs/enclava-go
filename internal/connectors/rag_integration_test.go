package connectors

import (
	"context"
	"encoding/json"
	"errors"
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

// connectorEmbeddingUpstream is the deterministic executor transport for the
// connector → rag ingest path; policy and pricing run inside the real
// shared executor, never a provider stub.
type connectorEmbeddingUpstream struct{}

func (p connectorEmbeddingUpstream) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	return nil, errors.New("unexpected chat call")
}

func (p connectorEmbeddingUpstream) CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	return nil, errors.New("unexpected chat stream call")
}

func (p connectorEmbeddingUpstream) DoResponsesRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	return nil, errors.New("unexpected responses call")
}

func (p connectorEmbeddingUpstream) DoResponsesStream(ctx context.Context, method, path string, body []byte) (io.ReadCloser, error) {
	return nil, errors.New("unexpected responses stream call")
}

func (p connectorEmbeddingUpstream) CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error) {
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
			"embedding": []float64{float64(len(input)), float64(strings.Count(input, "connector") + 1), 1},
		})
	}
	return json.Marshal(map[string]any{
		"object": "list",
		"data":   data,
		"usage":  map[string]any{"prompt_tokens": len(inputs), "total_tokens": len(inputs)},
	})
}

// newConnectorExecutor composes the real shared executor with the seeded
// embeddings endpoint binding and input-unit price the ingest attempt must
// resolve through.
func newConnectorExecutor(t *testing.T, db *postgres.DB, tenant string) *inference.Service {
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
	if err := billingRepo.PutPrice(ctx, tenant, "p-embed-input", billing.Price{
		SubjectType: billing.PriceSubjectEndpoint, SubjectID: "ep-embed",
		Capability: inference.CapabilityEmbeddings, Unit: billing.UnitInput,
		RateMicros: 1, UnitQuantity: 1, Currency: "USD",
	}, "tester", time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("put price: %v", err)
	}
	if err := billingRepo.PutPrice(ctx, tenant, "p-embed-cached", billing.Price{
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
		return connectorEmbeddingUpstream{}, nil
	}))
	if err != nil {
		t.Fatalf("new inference service: %v", err)
	}
	return svc
}

type connectorVectorStore struct {
	points map[string][]rag.QdrantPoint
}

func newConnectorVectorStore() *connectorVectorStore {
	return &connectorVectorStore{points: map[string][]rag.QdrantPoint{}}
}

func (s *connectorVectorStore) EnsureCollection(ctx context.Context, collection string, vectorSize int) error {
	if _, ok := s.points[collection]; !ok {
		s.points[collection] = nil
	}
	return nil
}

func (s *connectorVectorStore) UpsertPoints(ctx context.Context, collection string, points []rag.QdrantPoint) error {
	s.points[collection] = append(s.points[collection], points...)
	return nil
}

func (s *connectorVectorStore) DeleteDocumentPoints(ctx context.Context, collection, documentID string) error {
	kept := []rag.QdrantPoint{}
	for _, point := range s.points[collection] {
		if point.Payload["document_id"] != documentID {
			kept = append(kept, point)
		}
	}
	s.points[collection] = kept
	return nil
}

func (s *connectorVectorStore) DeleteCollection(ctx context.Context, collection string) error {
	delete(s.points, collection)
	return nil
}

func (s *connectorVectorStore) Search(ctx context.Context, collection string, vector []float64, limit int, filter map[string]any) ([]rag.QdrantSearchPoint, error) {
	out := []rag.QdrantSearchPoint{}
	for _, point := range s.points[collection] {
		out = append(out, rag.QdrantSearchPoint{ID: point.ID, Score: 0.99, Payload: point.Payload})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func TestConnectorSyncIndexesDocumentsIntoSearchableRAG(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "conn-rag")
	user := testdb.SeedUser(t, db, tenant, "conn-rag@example.test", nil)
	p := auth.Principal{TenantID: tenant, ActorID: user.ID, ActorKind: auth.ActorKindUser, UserID: user.ID,
		CredentialID: "key_test", CredentialKind: auth.CredentialKindAPIKey,
		AllowedModelsAll: true, AllowedEndpointsAll: true}

	jobsRepo := postgres.NewJobsRepo(db)
	jobsSvc := jobs.NewService(jobsRepo, 0)
	artifactStore, err := artifacts.NewStore(t.TempDir(), t.TempDir(), postgres.NewArtifactRepo(db))
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	ragSvc := rag.NewService(postgres.NewRAGRepo(db), newConnectorVectorStore(), newConnectorExecutor(t, db, tenant), "embed-test", artifactStore, jobsSvc)
	collection, err := ragSvc.CreateCollection(ctx, p, rag.CreateCollectionInput{
		Name:         "Connector Search",
		ChunkSize:    100,
		ChunkOverlap: 10,
		ACL:          map[string]any{"public": true},
	})
	if err != nil {
		t.Fatalf("create rag collection: %v", err)
	}
	connectorSvc := NewService(postgres.NewSourceRepo(db), jobsSvc, jobsRepo, ragSvc)
	source, err := connectorSvc.CreateSource(ctx, p, CreateSourceInput{
		Type:               SourceTypeSlack,
		Name:               "Slack Knowledge",
		TargetCollectionID: collection.ID,
		Config: map[string]any{
			"documents": []any{
				map[string]any{
					"file_name":  "thread.txt",
					"mime_type":  "text/plain",
					"content":    "connector rag searchable acceptance phrase",
					"source_id":  "thread-acceptance",
					"source_uri": "slack://thread-acceptance",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("create connector source: %v", err)
	}
	job, err := connectorSvc.SyncSource(ctx, p, source.ID)
	if err != nil {
		t.Fatalf("sync connector source: %v", err)
	}
	if job.Status != JobStatusQueued {
		t.Fatalf("expected queued sync job, got %#v", job)
	}

	// Drain the queued jobs: connector.sync then the rag.embed job the sync
	// produced for the uploaded document.
	for range 4 {
		claim, err := jobsSvc.Lease(ctx, "test-worker", time.Minute)
		if err != nil {
			t.Fatalf("lease: %v", err)
		}
		if claim == nil {
			break
		}
		var resultRef string
		var runErr error
		switch claim.Job().Operation {
		case OperationSync:
			resultRef, runErr = connectorSvc.JobHandler()(ctx, claim, nil)
		case rag.OperationEmbed:
			resultRef, runErr = ragSvc.JobHandler()(ctx, claim, nil)
		default:
			t.Fatalf("unexpected job operation %q", claim.Job().Operation)
		}
		if runErr != nil {
			_ = claim.Fail(ctx, runErr, jobs.FailureFatal)
			t.Fatalf("job %s failed: %v", claim.Job().Operation, runErr)
		}
		if _, err := claim.Complete(ctx, resultRef); err != nil {
			t.Fatalf("complete: %v", err)
		}
	}

	results, err := ragSvc.Search(ctx, p, rag.SearchInput{
		CollectionID: collection.ID,
		Query:        "acceptance phrase",
		Limit:        5,
	})
	if err != nil {
		t.Fatalf("search rag: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("expected connector-ingested RAG result")
	}
	if !strings.Contains(results[0].Content, "connector rag searchable acceptance phrase") {
		t.Fatalf("unexpected search content: %#v", results[0])
	}
	if results[0].Metadata["connector_source_id"] != source.ID || results[0].Metadata["connector_type"] != SourceTypeSlack {
		t.Fatalf("missing connector metadata in search result: %#v", results[0].Metadata)
	}
}
