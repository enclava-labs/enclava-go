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
	// The 09-10 sweep deleted the old layout-nav template; the same gating
	// intent is asserted on the dashboardNav model — a missing capability
	// never renders its item.
	hrefs := func(caps dashboardCaps) map[string]bool {
		t.Helper()
		out := map[string]bool{}
		for _, g := range dashboardNav(caps, "") {
			for _, it := range g.Items {
				out[it.Href] = true
			}
		}
		return out
	}
	if hrefs(dashboardCaps{})["/dashboard/knowledge"] {
		t.Fatal("knowledge navigation visible without a readable scope")
	}
	if !hrefs(dashboardCaps{Search: true})["/dashboard/knowledge"] {
		t.Fatal("knowledge navigation missing for readable principal")
	}
	if !hrefs(dashboardCaps{SourcesManage: true})["/dashboard/sources"] {
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
	ownerKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "knowledge-owner", OwnerUserID: owner.ID, Scopes: []string{"rag", "rag.manage"}})
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
	// Forged provenance fields — the upload surface is server-owned, so
	// neither the legacy dashboard form nor the /api/v1 route may carry them
	// into the stored document.
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
	uploadRequest := httptest.NewRequest(http.MethodPost, "/api/v1/rag/collections/"+collection.ID+"/documents", &uploadBody)
	uploadRequest.Header.Set("Content-Type", form.FormDataContentType())
	uploadRequest.Header.Set("Authorization", "Bearer "+ownerKey)
	uploadResponse := httptest.NewRecorder()
	app.Handler().ServeHTTP(uploadResponse, uploadRequest)
	if uploadResponse.Code != http.StatusOK && uploadResponse.Code != http.StatusAccepted {
		t.Fatalf("upload status=%d body=%s", uploadResponse.Code, uploadResponse.Body.String())
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
	if err := source.NewAuthorizer(sources).CanReadVersion(ctx, ownerPrincipal, inputObject, inputVersion, source.ReadOptions{}); err != nil {
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

// TestKnowledgeImportExtractionResultHTTP: "Add to knowledge" accepts a
// completed extract.process job ID through the public import route, preserving
// the generated provenance; re-import returns the same document.
func TestKnowledgeImportExtractionResultHTTP(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	identityRepo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "knowledge-extract")
	owner := testdb.SeedUser(t, db, tenant, "extract-owner@example.test", nil)
	outsider := testdb.SeedUser(t, db, tenant, "extract-outsider@example.test", nil)
	ownerKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "extract-owner", OwnerUserID: owner.ID, Scopes: []string{"rag", "rag.manage"}})
	outsiderKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "extract-outsider", OwnerUserID: outsider.ID, Scopes: []string{"rag", "rag.manage"}})
	jobsSvc := jobs.NewService(postgres.NewJobsRepo(db), 0)
	artifactRepo := postgres.NewArtifactRepo(db)
	store, err := artifacts.NewStore(t.TempDir(), t.TempDir(), artifactRepo)
	if err != nil {
		t.Fatal(err)
	}
	// The extraction input stays readable: an owner-mode source object.
	w, _, err := store.Begin(ctx, artifacts.Meta{Ref: artifacts.Ref{TenantID: tenant, Kind: artifacts.KindOriginal}, MimeType: "text/plain"}, time.Minute, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("extraction input body")); err != nil {
		t.Fatal(err)
	}
	if err := w.Finalize(ctx); err != nil {
		t.Fatal(err)
	}
	inputRef := w.Ref()
	sources := postgres.NewSourceRepo(db)
	if _, err := sources.UpsertObject(ctx, source.Object{TenantID: tenant, ID: "extract-input", Kind: "upload", NativeID: "extract-native",
		AuthorizationMode: source.AuthorizationOwner, OwnerActorID: owner.ID, ObservedSeq: 1}); err != nil {
		t.Fatal(err)
	}
	if err := sources.InsertObjectVersion(ctx, tenant, source.ObjectVersion{ObjectID: "extract-input", ObservedSeq: 1,
		ArtifactRef: inputRef.ID, SHA256: inputRef.SHA256, ByteSize: inputRef.ByteSize, MimeType: "text/plain", OriginalFilename: "input.txt"}); err != nil {
		t.Fatal(err)
	}
	// The completed result artifact of the extract.process job.
	rw, _, err := store.Begin(ctx, artifacts.Meta{Ref: artifacts.Ref{TenantID: tenant, Kind: artifacts.KindResult}, MimeType: "application/json"}, time.Minute, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Write([]byte(`{"id":"res_x","data":{"summary":"done"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := rw.Finalize(ctx); err != nil {
		t.Fatal(err)
	}
	resultRef := rw.Ref()

	ragSvc := rag.NewService(postgres.NewRAGRepo(db), nil, nil, "", store, jobsSvc)
	authService := auth.NewService(identityRepo, "")
	// Enqueue and import must bind the same submitter: resolve the exact
	// principal the HTTP route authenticates.
	ownerPrincipal, err := authService.Authenticate(ctx, ownerKey)
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := jobsSvc.Enqueue(ctx, ownerPrincipal,
		jobs.EnqueueRequest{TenantID: tenant, Operation: "extract.process", IdempotencyKey: "extract-http-1", InputRef: inputRef.ID, Policy: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := jobsSvc.Lease(ctx, "test-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatal("no extract job claim")
	}
	if _, err := claim.Complete(ctx, resultRef.ID); err != nil {
		t.Fatal(err)
	}
	collection, err := ragSvc.CreateCollection(ctx, ownerPrincipal, rag.CreateCollectionInput{Name: "extract-imports", ChunkSize: rag.DefaultChunkSize, ChunkOverlap: rag.DefaultChunkOverlap})
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp(authService, nil, "", nil, identityRepo, nil, nil, nil, nil, nil, nil, nil, nil, "sha256", "", "enc_", nil, nil, 1, false, nil, nil, GateConfig{}, sources, artifactRepo)
	app.ragSvc = ragSvc
	request := func(key, payload string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/imports", bytes.NewBufferString(payload))
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	importBody := `{"collection_id":"` + collection.ID + `","result_ref":"` + job.ID + `"}`
	// Another user's job is indistinguishable from absent.
	if got := request(outsiderKey, importBody); got.Code != http.StatusNotFound {
		t.Fatalf("non-owned extraction import = %d body=%s", got.Code, got.Body.String())
	}
	first := request(ownerKey, importBody)
	if first.Code != http.StatusAccepted {
		t.Fatalf("extraction import = %d body=%s", first.Code, first.Body.String())
	}
	var imported struct {
		Document rag.Document `json:"document"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &imported); err != nil {
		t.Fatal(err)
	}
	if imported.Document.ID == "" || imported.Document.SourceType != "generated" || imported.Document.FileName != "extraction-result.json" {
		t.Fatalf("imported extraction document: %#v", imported.Document)
	}
	docs, err := ragSvc.ListDocuments(ctx, ownerPrincipal, collection.ID, rag.DocumentStatusAll, 10, 0)
	if err != nil || len(docs) != 1 || docs[0].ID != imported.Document.ID {
		t.Fatalf("collection documents=%#v err=%v", docs, err)
	}
	second := request(ownerKey, importBody)
	if second.Code != http.StatusAccepted {
		t.Fatalf("duplicate import = %d body=%s", second.Code, second.Body.String())
	}
	var duplicate struct {
		Document rag.Document `json:"document"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &duplicate); err != nil {
		t.Fatal(err)
	}
	if duplicate.Document.ID != imported.Document.ID {
		t.Fatalf("duplicate import created %s, want the existing %s", duplicate.Document.ID, imported.Document.ID)
	}
}

// TestCollectionProfileVersionHTTP: the §6.7 selector validates and persists
// chunk_profile_version_id on create and update; unknown versions are 400.
func TestCollectionProfileVersionHTTP(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	identityRepo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "rag-profiles")
	manager := testdb.SeedUser(t, db, tenant, "profiles-manager@example.test", nil)
	managerKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "profiles-manager", OwnerUserID: manager.ID, Scopes: []string{"rag", "rag.manage"}})
	artifactRepo := postgres.NewArtifactRepo(db)
	ragSvc := rag.NewService(postgres.NewRAGRepo(db), nil, nil, "", nil, nil)
	authService := auth.NewService(identityRepo, "")
	app := NewApp(authService, nil, "", nil, identityRepo, nil, nil, nil, nil, nil, nil, nil, nil, "sha256", "", "enc_", nil, nil, 1, false, nil, nil, GateConfig{}, postgres.NewSourceRepo(db), artifactRepo)
	app.ragSvc = ragSvc
	profileA, err := postgres.NewChunkProfilesRepo(db).Create(ctx, tenant, source.ChunkProfile{
		ProfileID: "selector-a", SizeCodePoints: 500, OverlapCodePoints: 50,
		BoundaryStrategy: "heading", TableHandling: "atomic",
	}, manager.ID)
	if err != nil {
		t.Fatal(err)
	}
	profileB, err := postgres.NewChunkProfilesRepo(db).Create(ctx, tenant, source.ChunkProfile{
		ProfileID: "selector-b", SizeCodePoints: 900, OverlapCodePoints: 60,
		BoundaryStrategy: "fixed", TableHandling: "row-wise",
	}, manager.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, payload string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+managerKey)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	// Unknown versions are caller errors on both create and update.
	if got := request(http.MethodPost, "/api/v1/rag/collections", `{"name":"bad","chunk_profile_version_id":"cpv_missing"}`); got.Code != http.StatusBadRequest {
		t.Fatalf("create with unknown profile = %d body=%s", got.Code, got.Body.String())
	}
	created := request(http.MethodPost, "/api/v1/rag/collections", `{"name":"profiled","chunk_profile_version_id":"`+profileA.ID+`"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create with profile = %d body=%s", created.Code, created.Body.String())
	}
	var collection rag.Collection
	if err := json.Unmarshal(created.Body.Bytes(), &collection); err != nil {
		t.Fatal(err)
	}
	if collection.ChunkProfileVersionID != profileA.ID {
		t.Fatalf("created collection profile = %q, want %q", collection.ChunkProfileVersionID, profileA.ID)
	}
	if got := request(http.MethodPut, "/api/v1/rag/collections/"+collection.ID, `{"chunk_profile_version_id":"cpv_missing"}`); got.Code != http.StatusBadRequest {
		t.Fatalf("update with unknown profile = %d body=%s", got.Code, got.Body.String())
	}
	updated := request(http.MethodPut, "/api/v1/rag/collections/"+collection.ID, `{"chunk_profile_version_id":"`+profileB.ID+`"}`)
	if updated.Code != http.StatusOK {
		t.Fatalf("update with profile = %d body=%s", updated.Code, updated.Body.String())
	}
	var updatedCollection rag.Collection
	if err := json.Unmarshal(updated.Body.Bytes(), &updatedCollection); err != nil {
		t.Fatal(err)
	}
	if updatedCollection.ChunkProfileVersionID != profileB.ID {
		t.Fatalf("updated collection profile = %q, want %q", updatedCollection.ChunkProfileVersionID, profileB.ID)
	}
}

// TestCollectionChunkStrategyHTTP: chunk_strategy round-trips through the
// collection API — default auto on create, explicit fixed, PUT switches it,
// and invalid values are 400s; a pinned profile version reports read-only.
func TestCollectionChunkStrategyHTTP(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	identityRepo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "rag-chunk-strategy")
	manager := testdb.SeedUser(t, db, tenant, "chunk-manager@example.test", nil)
	managerKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "chunk-manager", OwnerUserID: manager.ID, Scopes: []string{"rag", "rag.manage"}})
	ragSvc := rag.NewService(postgres.NewRAGRepo(db), nil, nil, "", nil, nil)
	authService := auth.NewService(identityRepo, "")
	app := NewApp(authService, nil, "", nil, identityRepo, nil, nil, nil, nil, nil, nil, nil, nil, "sha256", "", "enc_", nil, nil, 1, false, nil, nil, GateConfig{}, postgres.NewSourceRepo(db), postgres.NewArtifactRepo(db))
	app.ragSvc = ragSvc
	request := func(method, path, payload string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+managerKey)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	readStrategy := func(rec *httptest.ResponseRecorder) string {
		t.Helper()
		var collection rag.Collection
		if err := json.Unmarshal(rec.Body.Bytes(), &collection); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
		return collection.ChunkStrategy
	}

	created := request(http.MethodPost, "/api/v1/rag/collections", `{"name":"default auto"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d body=%s", created.Code, created.Body.String())
	}
	if got := readStrategy(created); got != rag.ChunkStrategyAuto {
		t.Fatalf("default create strategy = %q, want auto", got)
	}
	var collection rag.Collection
	if err := json.Unmarshal(created.Body.Bytes(), &collection); err != nil {
		t.Fatal(err)
	}

	if got := request(http.MethodPost, "/api/v1/rag/collections", `{"name":"bad","chunk_strategy":"heading"}`); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid create strategy = %d body=%s", got.Code, got.Body.String())
	}
	if got := request(http.MethodPut, "/api/v1/rag/collections/"+collection.ID, `{"chunk_strategy":"clause"}`); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid update strategy = %d body=%s", got.Code, got.Body.String())
	}

	updated := request(http.MethodPut, "/api/v1/rag/collections/"+collection.ID, `{"chunk_strategy":"fixed"}`)
	if updated.Code != http.StatusOK {
		t.Fatalf("update to fixed = %d body=%s", updated.Code, updated.Body.String())
	}
	if got := readStrategy(updated); got != rag.ChunkStrategyFixed {
		t.Fatalf("updated strategy = %q, want fixed", got)
	}
	fetched := request(http.MethodGet, "/api/v1/rag/collections/"+collection.ID, "")
	if got := readStrategy(fetched); got != rag.ChunkStrategyFixed {
		t.Fatalf("get strategy = %q, want fixed", got)
	}

	// A pinned document-profile version reports its own strategy read-only.
	profile, err := postgres.NewChunkProfilesRepo(db).Create(ctx, tenant, source.ChunkProfile{
		ProfileID: "strategy-heading", SizeCodePoints: 500, OverlapCodePoints: 50,
		BoundaryStrategy: "heading", TableHandling: "atomic",
	}, manager.ID)
	if err != nil {
		t.Fatal(err)
	}
	pinned := request(http.MethodPost, "/api/v1/rag/collections", `{"name":"pinned","chunk_profile_version_id":"`+profile.ID+`"}`)
	if pinned.Code != http.StatusCreated {
		t.Fatalf("create pinned = %d body=%s", pinned.Code, pinned.Body.String())
	}
	if got := readStrategy(pinned); got != "heading" {
		t.Fatalf("pinned strategy = %q, want heading", got)
	}
}
