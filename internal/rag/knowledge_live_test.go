package rag_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/connectors"
	"enclava-go/internal/jobs"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

const liveCorpus = "legal exception orchid chat transcript retention memorandum"

type corpusAdapter struct {
	content  string
	revision string
}

func (a *corpusAdapter) Capabilities() connectors.AdapterCapabilities {
	return connectors.AdapterCapabilities{Content: true}
}
func (a *corpusAdapter) Content(context.Context, source.Connection, connectors.Cursor, int) (connectors.ContentPage, error) {
	return connectors.ContentPage{Items: []connectors.ContentItem{{
		NativeID: "corpus-document", Kind: "document", NativeRevision: a.revision,
		Filename: "corpus.txt", MIMEType: "text/plain", Content: []byte(a.content),
		URL: "https://fixture.invalid/corpus-document",
	}}, Completeness: connectors.CompletenessComplete, Snapshot: true}, nil
}

func TestLiveKnowledgeCorpusSyncPublicationRetrievalAndRevocation(t *testing.T) {
	qdrantURL := envOrFatal(t, "QDRANT_INTEGRATION_URL")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "knowledge-live")
	user := testdb.SeedUserWithGrants(t, db, tenant, "knowledge-live@example.test", []string{"rag", "connectors.manage"}, auth.ResourceGrants{
		Models: auth.Allowlist{All: true}, Endpoints: auth.Allowlist{All: true},
	})
	p := auth.Principal{
		TenantID: tenant, ActorID: user.ID, ActorKind: auth.ActorKindUser, UserID: user.ID,
		CredentialID: "knowledge-live-key", CredentialKind: auth.CredentialKindAPIKey,
		BillingSubjectID: user.ID, BillingSubjectKind: auth.BillingSubjectUser,
		AllowedModelsAll: true, AllowedEndpointsAll: true,
		Scopes: map[string]struct{}{"rag": {}, "connectors.manage": {}},
	}

	jobsRepo := postgres.NewJobsRepo(db)
	jobsSvc := jobs.NewService(jobsRepo, 0)
	artifactRepo := postgres.NewArtifactRepo(db)
	artifactStore, err := artifacts.NewStore(t.TempDir(), t.TempDir(), artifactRepo)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	vector := rag.NewQdrantClient(qdrantURL, 15*time.Second)
	ragRepo := postgres.NewRAGRepo(db)
	ragSvc := rag.NewService(ragRepo, vector, newExecutor(t, db, tenant, &fakeEmbeddingUpstream{}), "embed-test", artifactStore, jobsSvc)
	collection, err := ragSvc.CreateCollection(ctx, p, rag.CreateCollectionInput{
		Name: "Live corpus " + ragSafeName(), ChunkSize: 240, ChunkOverlap: 30,
		ACL: map[string]any{"public": true},
	})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	defer func() { _ = vector.DeleteCollection(context.Background(), collection.QdrantCollection) }()

	sourceRepo := postgres.NewSourceRepo(db)
	connection := source.Connection{
		TenantID: tenant, ID: "connection-live-corpus", Kind: source.ConnectionSlack,
		Name: "Local corpus fixture", CredentialRef: "CONNECTOR_SLACK_FIXTURE",
		Config: map[string]any{}, TargetCollectionID: collection.ID,
		Status: "active", CreatedBy: user.ID,
	}
	if err := sourceRepo.CreateConnection(ctx, connection); err != nil {
		t.Fatalf("create fixture connection: %v", err)
	}
	reader := testdb.SeedUserWithGrants(t, db, tenant, "knowledge-live-reader@example.test", []string{"rag"}, auth.ResourceGrants{
		Models: auth.Allowlist{All: true}, Endpoints: auth.Allowlist{All: true},
	})
	readerP := auth.Principal{
		TenantID: tenant, ActorID: reader.ID, ActorKind: auth.ActorKindUser, UserID: reader.ID,
		CredentialID: "knowledge-live-reader-key", CredentialKind: auth.CredentialKindAPIKey,
		BillingSubjectID: reader.ID, BillingSubjectKind: auth.BillingSubjectUser,
		AllowedModelsAll: true, AllowedEndpointsAll: true,
		Scopes: map[string]struct{}{"rag": {}},
	}

	adapter := &corpusAdapter{content: liveCorpus, revision: "fixture-v1"}
	connectorSvc := connectors.NewService(sourceRepo, jobsSvc, jobsRepo)
	connectorSvc.SetPrincipalResolver(auth.NewService(postgres.NewIdentityRepo(db), "").PrincipalForActor)
	connectorSvc.SetSyncDependencies(connectors.AdapterFactories{
		connectors.SourceKindSlack: func(source.Connection) (connectors.Adapter, error) { return adapter, nil },
	}, postgres.NewConnectorObservationsRepo(db), artifactStore, sourceRepo)
	if _, err := connectorSvc.SyncSourceWindowed(ctx, auth.Principal{TenantID: tenant, ActorID: user.ID}, connection.ID, 1); err != nil {
		t.Fatalf("schedule initial fixture sync: %v", err)
	}
	runLiveKnowledgeJobs(t, ctx, jobsSvc, connectorSvc, ragSvc)

	hybrid, err := ragSvc.Search(ctx, p, rag.SearchInput{CollectionID: collection.ID, Query: "orchid legal exception", Mode: rag.QueryHybrid, Limit: 5})
	if err != nil || len(hybrid) == 0 {
		t.Fatalf("hybrid search returned no fixture result: %#v err=%v", hybrid, err)
	}
	if hybrid[0].Content == "" || len(hybrid[0].Spans) == 0 || hybrid[0].Spans[0].ByteStart < 0 || hybrid[0].Spans[0].ByteEnd <= hybrid[0].Spans[0].ByteStart || !strings.Contains(hybrid[0].Content, "orchid") {
		t.Fatalf("hybrid result has invalid snippet/span boundaries: %#v", hybrid[0])
	}
	if hybrid[0].EvidenceID == "" || hybrid[0].SourceURI != "https://fixture.invalid/corpus-document" {
		t.Fatalf("hybrid result missing evidence or native citation: %#v", hybrid[0])
	}
	lexical, err := ragSvc.Search(ctx, p, rag.SearchInput{CollectionID: collection.ID, Query: "retention memorandum", Mode: rag.QueryLexical, Limit: 5})
	if err != nil || len(lexical) == 0 || lexical[0].DocumentID != hybrid[0].DocumentID {
		t.Fatalf("lexical search did not return the published fixture: %#v err=%v", lexical, err)
	}
	resolved, err := ragSvc.ResolveEvidence(ctx, p, []string{hybrid[0].EvidenceID})
	if err != nil || len(resolved) != 1 || resolved[0].Status != "current" || resolved[0].RetainedCopyLabel != "retained copy" {
		t.Fatalf("evidence resolution: %#v err=%v", resolved, err)
	}
	unknownEvidenceErr := func() error {
		_, err := ragSvc.ResolveEvidence(ctx, p, []string{"forged-live-evidence-id"})
		return err
	}()
	if !errors.Is(unknownEvidenceErr, source.ErrDenied) {
		t.Fatalf("unknown evidence did not use uniform not-found outcome: %v", unknownEvidenceErr)
	}

	adapter.content = liveCorpus + " sapphire manual revision"
	adapter.revision = "fixture-v2"
	if _, err := connectorSvc.SyncSource(ctx, p, connection.ID); err != nil {
		t.Fatalf("schedule manual fixture sync: %v", err)
	}
	runLiveKnowledgeJobs(t, ctx, jobsSvc, connectorSvc, ragSvc)
	manual, err := ragSvc.Search(ctx, p, rag.SearchInput{CollectionID: collection.ID, Query: "sapphire", Mode: rag.QueryLexical, Limit: 5})
	if err != nil || len(manual) == 0 || manual[0].DocumentID != hybrid[0].DocumentID || !strings.Contains(manual[0].Content, "sapphire") {
		t.Fatalf("manual sync did not publish the updated document: %#v err=%v", manual, err)
	}

	// Access follows the collection: a reader of the public collection finds
	// the synced document; making the collection private revokes it.
	if shared, err := ragSvc.Search(ctx, readerP, rag.SearchInput{CollectionID: collection.ID, Query: "orchid legal exception", Mode: rag.QueryHybrid, Limit: 5}); err != nil || len(shared) == 0 {
		t.Fatalf("collection reader did not find the synced document: %#v err=%v", shared, err)
	}
	if _, err := ragSvc.UpdateCollection(ctx, p, collection.ID, rag.UpdateCollectionInput{ACL: map[string]any{"public": false}}); err != nil {
		t.Fatalf("make collection private: %v", err)
	}
	if revokedResults, err := ragSvc.Search(ctx, readerP, rag.SearchInput{CollectionID: collection.ID, Query: "orchid legal exception", Mode: rag.QueryHybrid, Limit: 5}); !errors.Is(err, source.ErrDenied) || len(revokedResults) != 0 {
		t.Fatalf("revoked reader still searches the collection: %#v err=%v", revokedResults, err)
	}
	_, revokedEvidenceErr := ragSvc.ResolveEvidence(ctx, readerP, []string{hybrid[0].EvidenceID})
	if !errors.Is(revokedEvidenceErr, unknownEvidenceErr) {
		t.Fatalf("revoked and unknown evidence outcomes differ: revoked=%v unknown=%v", revokedEvidenceErr, unknownEvidenceErr)
	}
}

func runLiveKnowledgeJobs(t *testing.T, ctx context.Context, jobsSvc *jobs.Service, connectorSvc *connectors.Service, ragSvc *rag.Service) {
	t.Helper()
	handlers := map[string]jobs.HandlerFunc{
		connectors.OperationSync: connectorSvc.JobHandler(),
		rag.OperationIngest:      ragSvc.IngestJobHandler(),
		rag.OperationEmbed:       ragSvc.JobHandler(),
	}
	for range 32 {
		claim, err := jobsSvc.Lease(ctx, "knowledge-live-worker", time.Minute)
		if err != nil {
			t.Fatalf("lease live pipeline job: %v", err)
		}
		if claim == nil {
			return
		}
		handler := handlers[claim.Job().Operation]
		if handler == nil {
			t.Fatalf("unexpected pipeline operation %q", claim.Job().Operation)
		}
		result, err := handler(ctx, claim, nil)
		if err != nil {
			_ = claim.Fail(ctx, err, jobs.FailureFatal)
			t.Fatalf("run %s: %v", claim.Job().Operation, err)
		}
		if _, err := claim.Complete(ctx, result); err != nil {
			t.Fatalf("complete %s: %v", claim.Job().Operation, err)
		}
	}
	t.Fatal("live knowledge pipeline exceeded its job bound")
}
