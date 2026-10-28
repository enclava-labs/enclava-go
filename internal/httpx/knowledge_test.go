package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
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

func TestKnowledgeNavigationRequiresReadableScope(t *testing.T) {
	render := func(readable, manage bool) string {
		t.Helper()
		var out bytes.Buffer
		err := dashboardTemplates.ExecuteTemplate(&out, "layout-nav", dashboardPageCommon{
			KnowledgeReadable: readable,
			ConnectorsManage:  manage,
		})
		if err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if strings.Contains(render(false, false), `href="/dashboard/rag"`) {
		t.Fatal("knowledge navigation visible without a readable scope")
	}
	if !strings.Contains(render(true, false), `>Knowledge</a>`) {
		t.Fatal("knowledge navigation missing for readable principal")
	}
	if !strings.Contains(render(false, true), `href="/dashboard/connectors"`) {
		t.Fatal("source navigation missing for connector manager")
	}
}

func TestKnowledgeImportHTTPAuthorizationAndDuplicate(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	identityRepo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "knowledge-import")
	owner := testdb.SeedUser(t, db, tenant, "knowledge-owner@example.test", nil)
	outsider := testdb.SeedUser(t, db, tenant, "knowledge-outsider@example.test", nil)
	ownerKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "knowledge-owner", OwnerUserID: owner.ID, Scopes: []string{"rag"}})
	outsiderKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "knowledge-outsider", OwnerUserID: outsider.ID, Scopes: []string{"rag"}})
	jobsRepo := postgres.NewJobsRepo(db)
	jobsSvc := jobs.NewService(jobsRepo, 0)
	artifactRepo := postgres.NewArtifactRepo(db)
	store, err := artifacts.NewStore(t.TempDir(), t.TempDir(), artifactRepo)
	if err != nil {
		t.Fatal(err)
	}
	writer, _, err := store.Begin(ctx, artifacts.Meta{Ref: artifacts.Ref{TenantID: tenant, Kind: artifacts.KindOriginal}, MimeType: "text/plain"}, time.Minute, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("stored source evidence")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Finalize(ctx); err != nil {
		t.Fatal(err)
	}
	ref := writer.Ref()
	sources := postgres.NewSourceRepo(db)
	if _, err := sources.UpsertObject(ctx, source.Object{TenantID: tenant, ID: "stored-object", Kind: "document", NativeID: "stored-native", AuthorizationMode: source.AuthorizationOwner, OwnerActorID: owner.ID, ObservedSeq: 1}); err != nil {
		t.Fatal(err)
	}
	if err := sources.InsertObjectVersion(ctx, tenant, source.ObjectVersion{ObjectID: "stored-object", ObservedSeq: 1, ArtifactRef: ref.ID, SHA256: ref.SHA256, ByteSize: ref.ByteSize, MimeType: "text/plain", OriginalFilename: "stored.txt"}); err != nil {
		t.Fatal(err)
	}
	ragSvc := rag.NewService(postgres.NewRAGRepo(db), nil, nil, "", store, jobsSvc)
	ownerPrincipal := auth.Principal{TenantID: tenant, ActorID: owner.ID, UserID: owner.ID, ActorKind: auth.ActorKindUser}
	collection, err := ragSvc.CreateCollection(ctx, ownerPrincipal, rag.CreateCollectionInput{Name: "imports", ChunkSize: rag.DefaultChunkSize, ChunkOverlap: rag.DefaultChunkOverlap})
	if err != nil {
		t.Fatal(err)
	}
	authService := auth.NewService(identityRepo, "")
	app := NewApp(authService, nil, "", nil, identityRepo, nil, nil, nil, nil, nil, nil, nil, nil, "sha256", "", "enc_", nil, nil, 1, false, nil, nil, GateConfig{}, sources, artifactRepo)
	app.ragSvc = ragSvc
	request := func(key string, payload map[string]string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/imports", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	filterRequest := httptest.NewRequest(http.MethodPost, "/api/v1/rag/collections/"+collection.ID+"/search", bytes.NewBufferString(`{"query":"anything","filter":{"tenant_id":"forged"}}`))
	filterRequest.Header.Set("Authorization", "Bearer "+ownerKey)
	filterResponse := httptest.NewRecorder()
	app.Handler().ServeHTTP(filterResponse, filterRequest)
	if filterResponse.Code != http.StatusBadRequest {
		t.Fatalf("raw filter map was accepted: %d %s", filterResponse.Code, filterResponse.Body.String())
	}
	var uploadBody bytes.Buffer
	form := multipart.NewWriter(&uploadBody)
	_ = form.WriteField("action", "upload_document")
	_ = form.WriteField("collection_id", collection.ID)
	_ = form.WriteField("source_type", "forged")
	_ = form.WriteField("source_id", "forged-id")
	_ = form.WriteField("source_uri", "https://attacker.invalid")
	file, err := form.CreateFormFile("file", "upload.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("untrusted upload content")); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	uploadRequest := httptest.NewRequest(http.MethodPost, "/dashboard/partials/rag", &uploadBody)
	uploadRequest.Header.Set("Content-Type", form.FormDataContentType())
	if success, failure := app.handleDashboardRAGAction(uploadRequest, ownerPrincipal); success == "" || failure != "" {
		t.Fatalf("upload action success=%q failure=%q", success, failure)
	}
	uploaded, err := ragSvc.ListDocuments(ctx, ownerPrincipal, collection.ID, rag.DocumentStatusAll, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(uploaded) != 1 || uploaded[0].SourceType != "upload" || uploaded[0].SourceID == "forged-id" || uploaded[0].SourceURI != "" {
		t.Fatalf("upload accepted caller provenance: %#v", uploaded)
	}
	artifactPayload := map[string]string{"collection_id": collection.ID, "artifact_ref": ref.ID}
	if got := request(outsiderKey, artifactPayload); got.Code != http.StatusNotFound {
		t.Fatalf("foreign import status=%d body=%s", got.Code, got.Body.String())
	}
	first := request(ownerKey, artifactPayload)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first import status=%d body=%s", first.Code, first.Body.String())
	}
	second := request(ownerKey, artifactPayload)
	if second.Code != http.StatusAccepted {
		t.Fatalf("duplicate import status=%d body=%s", second.Code, second.Body.String())
	}
	var a, b struct {
		Document rag.Document `json:"document"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if a.Document.ID == "" || a.Document.ID != b.Document.ID {
		t.Fatalf("duplicate created a second object: first=%q second=%q", a.Document.ID, b.Document.ID)
	}
	resultWriter, _, err := store.Begin(ctx, artifacts.Meta{Ref: artifacts.Ref{TenantID: tenant, Kind: artifacts.KindResult}, MimeType: "text/plain"}, time.Minute, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resultWriter.Write([]byte("stored preview result")); err != nil {
		t.Fatal(err)
	}
	if err := resultWriter.Finalize(ctx); err != nil {
		t.Fatal(err)
	}
	resultRef := resultWriter.Ref()
	jobPrincipal, err := authService.Authenticate(ctx, ownerKey)
	if err != nil {
		t.Fatal(err)
	}
	resultJob, _, err := jobsSvc.Enqueue(ctx, jobPrincipal, jobs.EnqueueRequest{
		TenantID: tenant, Operation: "document.preview", IdempotencyKey: "knowledge-result-fixture",
		InputRef: ref.ID, InputHash: ref.SHA256, Policy: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.ExecContext(ctx, `UPDATE jobs SET status='succeeded', result_ref=$3, finished_at=now() WHERE tenant_id=$1 AND id=$2`, tenant, resultJob.ID, resultRef.ID); err != nil {
		t.Fatal(err)
	}
	if job, err := jobsSvc.Get(ctx, jobPrincipal, resultJob.ID); err != nil || job.Status != "succeeded" {
		t.Fatalf("stored fixture job=%+v err=%v", job, err)
	}
	if result, err := postgres.NewRAGRepo(db).GetStoredResult(ctx, tenant, resultJob.ID); err != nil || result.Status != "succeeded" || result.Operation != "document.preview" || result.InputRef != ref.ID || result.ResultRef != resultRef.ID {
		t.Fatalf("stored fixture result=%+v err=%v", result, err)
	}
	inputObject, inputVersion, err := postgres.NewRAGRepo(db).FindArtifactObjectVersion(ctx, tenant, ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.NewAuthorizer(sources, 0).CanReadVersion(ctx, ownerPrincipal, inputObject, inputVersion, source.ReadOptions{}); err != nil {
		t.Fatalf("input artifact is not readable: %v", err)
	}
	if reader, err := store.Read(ctx, artifacts.Ref{TenantID: tenant, ID: resultRef.ID, Kind: artifacts.KindResult}); err != nil {
		t.Fatalf("result artifact is unreadable: %v", err)
	} else {
		_ = reader.Close()
	}
	resultPayload := map[string]string{"collection_id": collection.ID, "result_ref": resultJob.ID}
	if got := request(outsiderKey, resultPayload); got.Code != http.StatusNotFound {
		t.Fatalf("forbidden result import status=%d body=%s", got.Code, got.Body.String())
	}
	resultFirst := request(ownerKey, resultPayload)
	resultSecond := request(ownerKey, resultPayload)
	if resultFirst.Code != http.StatusAccepted || resultSecond.Code != http.StatusAccepted {
		t.Fatalf("stored result imports status=%d/%d body=%s/%s", resultFirst.Code, resultSecond.Code, resultFirst.Body.String(), resultSecond.Body.String())
	}
	var resultA, resultB struct {
		Document rag.Document `json:"document"`
	}
	if err := json.Unmarshal(resultFirst.Body.Bytes(), &resultA); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(resultSecond.Body.Bytes(), &resultB); err != nil {
		t.Fatal(err)
	}
	if resultA.Document.ID == "" || resultA.Document.ID != resultB.Document.ID {
		t.Fatalf("duplicate result import created a second object: %q != %q", resultA.Document.ID, resultB.Document.ID)
	}
	var count int
	if err := db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM knowledge_imports WHERE tenant_id=$1 AND collection_id=$2`, tenant, collection.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("knowledge import records=%d, want two deduplicated imports", count)
	}
}
