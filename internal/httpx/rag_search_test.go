package httpx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/jobs"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestRAGSearchRouteAuthAndErrors(t *testing.T) {
	db := testdb.New(t)
	repo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "bootstrap")
	user := testdb.SeedUser(t, db, tenant, "rag-search@example.com", nil)
	withScope, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "rag-search", OwnerUserID: user.ID, Scopes: []string{"rag"}})
	withoutScope, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "no-rag", OwnerUserID: user.ID, Scopes: []string{"models.list"}})
	authSvc := auth.NewService(repo, "")
	app := NewApp(authSvc, nil, "", nil, repo, nil, nil, nil, nil, nil, nil, nil, nil, "sha256", "", "enc_", nil, nil, 1, false, nil, nil, GateConfig{}, nil, nil)
	app.ragSvc = &rag.Service{}

	request := func(key, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/rag/collections/c1/search", bytes.NewBufferString(body))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	if got := request("", `{}`); got.Code != http.StatusUnauthorized {
		t.Fatalf("mounted route should require authentication, got %d", got.Code)
	}
	if got := request(withoutScope, `{}`); got.Code != http.StatusForbidden {
		t.Fatalf("route should enforce rag scope, got %d", got.Code)
	}
	if got := request(withScope, `not-json`); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid request should return 400, got %d body=%s", got.Code, got.Body.String())
	}
	app.ragSvc = nil
	if got := request(withScope, `{}`); got.Code != http.StatusNotImplemented {
		t.Fatalf("unconfigured service should return 501, got %d", got.Code)
	}
}

func TestRAGDocumentReprocessRoute(t *testing.T) {
	db := testdb.New(t)
	repo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "bootstrap")
	user := testdb.SeedUser(t, db, tenant, "reprocess@example.com", nil)
	manager, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "reprocess-mgr", OwnerUserID: user.ID, Scopes: []string{"rag", "rag.manage"}})
	reader, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "reprocess-read", OwnerUserID: user.ID, Scopes: []string{"rag"}})
	authSvc := auth.NewService(repo, "")
	app := NewApp(authSvc, nil, "", nil, repo, nil, nil, nil, nil, nil, nil, nil, nil, "sha256", "", "enc_", nil, nil, 1, false, nil, nil, GateConfig{}, nil, nil)
	app.ragSvc = &rag.Service{}

	request := func(key, method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	// rag alone is not enough: restaging produces billable embedding work.
	if got := request(reader, http.MethodPost, "/api/v1/rag/documents/doc_1/reprocess"); got.Code != http.StatusForbidden {
		t.Fatalf("reprocess should require rag.manage, got %d", got.Code)
	}
	// Mounted and dispatched into the service (zero Service errors → 500);
	// the pre-mount behavior was 404.
	if got := request(manager, http.MethodPost, "/api/v1/rag/documents/doc_1/reprocess"); got.Code != http.StatusInternalServerError {
		t.Fatalf("mounted reprocess should reach the service (500 on zero Service), got %d body=%s", got.Code, got.Body.String())
	}
	if got := request(manager, http.MethodGet, "/api/v1/rag/documents/doc_1/reprocess"); got.Code == http.StatusAccepted {
		t.Fatal("GET reprocess must never restage")
	}
	app.ragSvc = nil
	if got := request(manager, http.MethodPost, "/api/v1/rag/documents/doc_1/reprocess"); got.Code != http.StatusNotImplemented {
		t.Fatalf("unconfigured rag should answer 501, got %d", got.Code)
	}
}

// ragDocsFixture is a real rag service over the test database with API-key
// principals: owner (rag + rag.manage), member (rag, no audience grant) and
// an other-tenant caller.
type ragDocsFixture struct {
	app          *App
	db           *postgres.DB
	repo         *postgres.RAGRepo
	svc          *rag.Service
	tenant       string
	ownerID      string
	memberID     string
	collectionID string
	ownerKey     string
	memberKey    string
	otherKey     string
	request      func(method, path, key string) *httptest.ResponseRecorder
}

func seedRAGDocumentsApp(t *testing.T) ragDocsFixture {
	t.Helper()
	db := testdb.New(t)
	ctx := context.Background()
	identityRepo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "rag-docs")
	owner := testdb.SeedUser(t, db, tenant, "rag-docs-owner@example.test", nil)
	member := testdb.SeedUser(t, db, tenant, "rag-docs-member@example.test", nil)
	ownerKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "docs-owner", OwnerUserID: owner.ID, Scopes: []string{"rag", "rag.manage"}})
	memberKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "docs-member", OwnerUserID: member.ID, Scopes: []string{"rag"}})
	otherTenant := testdb.SeedTenant(t, db, "rag-docs-other")
	otherUser := testdb.SeedUser(t, db, otherTenant, "rag-docs-other@example.test", nil)
	otherKey, _ := testdb.SeedAPIKey(t, db, otherTenant, testdb.SeedKeySpec{Name: "docs-other", OwnerUserID: otherUser.ID, Scopes: []string{"rag"}})
	sources := postgres.NewSourceRepo(db)
	svc := rag.NewService(postgres.NewRAGRepo(db), nil, nil, "", nil, nil)
	authService := auth.NewService(identityRepo, "")
	app := NewApp(authService, nil, "", nil, identityRepo, nil, nil, nil, nil, nil, nil, nil, nil, "sha256", "", "enc_", nil, nil, 1, false, nil, nil, GateConfig{}, sources, nil)
	app.ragSvc = svc
	ownerPrincipal := auth.Principal{TenantID: tenant, ActorID: owner.ID, UserID: owner.ID, ActorKind: auth.ActorKindUser}
	collection, err := svc.CreateCollection(ctx, ownerPrincipal, rag.CreateCollectionInput{Name: "docs", ChunkSize: 200, ChunkOverlap: 20, ACL: map[string]any{"public": true}})
	if err != nil {
		t.Fatal(err)
	}
	f := ragDocsFixture{
		app: app, db: db, repo: postgres.NewRAGRepo(db), svc: svc, tenant: tenant,
		ownerID: owner.ID, memberID: member.ID, collectionID: collection.ID,
		ownerKey: ownerKey, memberKey: memberKey, otherKey: otherKey,
	}
	f.request = func(method, path, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	return f
}

// stageEvidenceObject persists an audience object + version in the fixture
// tenant/collection, stages one published generation with a span-backed
// chunk and saves an evidence row for it — the real retained-chunk shape the
// passage route reads.
func (f ragDocsFixture) stageEvidenceObject(t *testing.T, objectID, audienceOwnerID string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := f.repo.UpsertObject(ctx, source.Object{
		TenantID: f.tenant, ID: objectID, Kind: "upload", NativeID: "up_" + objectID,
		AuthorizationMode: source.AuthorizationAudience, OwnerActorID: audienceOwnerID,
		Audience: []string{audienceOwnerID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.InsertObjectVersion(ctx, f.tenant, source.ObjectVersion{
		ObjectID: objectID, ObservedSeq: 1, ArtifactRef: "art_" + objectID, SHA256: "sha_" + objectID,
		ByteSize: 22, MimeType: "text/plain", OriginalFilename: objectID + ".txt",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.AddCollectionItem(ctx, f.tenant, f.collectionID, objectID, audienceOwnerID); err != nil {
		t.Fatal(err)
	}
	versionID := objectID + "#1"
	parsed := source.Document{
		Ref: source.ObjectRef{TenantID: f.tenant, ObjectID: objectID, ObservedSeq: 1},
		Blocks: []source.Block{
			{ID: "blk_h_" + objectID, Ordinal: 0, Kind: "heading", Text: "Section H", ByteStart: 0, ByteEnd: 9, Locator: source.Locator{HeadingLevel: 1, Section: "Section H"}},
			{ID: "blk_p_" + objectID, Ordinal: 1, Kind: "paragraph", ParentID: "blk_h_" + objectID, Text: "passage body", ByteStart: 10, ByteEnd: 22, Locator: source.Locator{Page: new(7)}},
		},
	}
	if err := f.repo.SaveParsedDocument(ctx, "rag-structured-v1", parsed); err != nil {
		t.Fatal(err)
	}
	profileID, err := f.repo.EnsureChunkProfile(ctx, f.tenant, "rag-"+f.collectionID+"-structured", 200, 20)
	if err != nil {
		t.Fatal(err)
	}
	chunkID := "chk_" + objectID
	chunks := []rag.DocumentChunk{{
		ID: chunkID, DocumentID: objectID, CollectionID: f.collectionID, Generation: 1,
		ChunkIndex: 0, Content: "the retained passage text", TokenCount: 4,
		HeaderPath: "Section H",
		SpanRefs:   []source.Span{{Ref: parsed.Ref, BlockID: "blk_p_" + objectID, ByteStart: 0, ByteEnd: 5}},
	}}
	if err := f.repo.ReplaceVersionChunksAndStage(ctx, f.tenant, f.collectionID, objectID, versionID, 1, 1, profileID, chunks); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.MarkGenerationState(ctx, f.tenant, f.collectionID, versionID, 1, "published"); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.SaveEvidence(ctx, rag.EvidenceRecord{
		TenantID: f.tenant, ID: "ev_" + objectID, CollectionID: f.collectionID, ObjectID: objectID,
		ObjectVersionID: versionID, ObservedSeq: 1, Generation: 1, ChunkID: chunkID,
		SpanRefs: []source.Span{{Ref: parsed.Ref, BlockID: "blk_p_" + objectID, ByteStart: 0, ByteEnd: 5}},
	}); err != nil {
		t.Fatal(err)
	}
	return "ev_" + objectID
}

func TestRAGEvidencePassageRoute(t *testing.T) {
	f := seedRAGDocumentsApp(t)
	evidenceID := f.stageEvidenceObject(t, "obj_passage", f.ownerID)

	// Authorized caller gets the frozen passage shape with real content.
	resp := f.request(http.MethodGet, "/api/v1/rag/evidence/"+evidenceID+"/passage", f.ownerKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("authorized passage = %d body=%s", resp.Code, resp.Body.String())
	}
	var payload struct {
		EvidenceID   string        `json:"evidence_id"`
		DocumentID   string        `json:"document_id"`
		CollectionID string        `json:"collection_id"`
		Content      string        `json:"content"`
		Spans        []source.Span `json:"spans"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.EvidenceID != evidenceID || payload.DocumentID != "obj_passage" ||
		payload.CollectionID != f.collectionID || payload.Content != "the retained passage text" ||
		len(payload.Spans) != 1 || payload.Spans[0].BlockID != "blk_p_obj_passage" {
		t.Fatalf("passage payload mismatch: %#v", payload)
	}

	// Unknown, forbidden and other-tenant requests share one public 404.
	forbidden := f.request(http.MethodGet, "/api/v1/rag/evidence/"+evidenceID+"/passage", f.memberKey)
	unknown := f.request(http.MethodGet, "/api/v1/rag/evidence/ev_does_not_exist/passage", f.ownerKey)
	foreign := f.request(http.MethodGet, "/api/v1/rag/evidence/"+evidenceID+"/passage", f.otherKey)
	if forbidden.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound || foreign.Code != http.StatusNotFound {
		t.Fatalf("passage denials: forbidden=%d unknown=%d foreign=%d", forbidden.Code, unknown.Code, foreign.Code)
	}
	if forbidden.Body.String() != unknown.Body.String() || foreign.Body.String() != unknown.Body.String() {
		t.Fatalf("denial bodies must be indistinguishable: %s | %s | %s",
			forbidden.Body.String(), unknown.Body.String(), foreign.Body.String())
	}

	// Superseded citations keep their metadata but lose passage access.
	ctx := context.Background()
	if err := f.repo.MarkGenerationState(ctx, f.tenant, f.collectionID, "obj_passage#1", 1, "superseded"); err != nil {
		t.Fatal(err)
	}
	if got := f.request(http.MethodGet, "/api/v1/rag/evidence/"+evidenceID+"/passage", f.ownerKey); got.Code != http.StatusNotFound {
		t.Fatalf("superseded passage must 404, got %d body=%s", got.Code, got.Body.String())
	}
	metadata := f.request(http.MethodGet, "/api/v1/rag/evidence/"+evidenceID, f.ownerKey)
	if metadata.Code != http.StatusOK {
		t.Fatalf("superseded metadata = %d body=%s", metadata.Code, metadata.Body.String())
	}
	var resolution struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(metadata.Body.Bytes(), &resolution); err != nil {
		t.Fatal(err)
	}
	if resolution.Status != "superseded" {
		t.Fatalf("resolve status = %q, want superseded", resolution.Status)
	}

	// A tombstoned document denies passage identically.
	if err := f.repo.TombstoneObject(ctx, f.tenant, "obj_passage"); err != nil {
		t.Fatal(err)
	}
	tombstoned := f.request(http.MethodGet, "/api/v1/rag/evidence/"+evidenceID+"/passage", f.ownerKey)
	if tombstoned.Code != http.StatusNotFound || tombstoned.Body.String() != unknown.Body.String() {
		t.Fatalf("tombstoned passage = %d body=%s, want the shared 404", tombstoned.Code, tombstoned.Body.String())
	}
}

func TestRAGDocumentListStatusRoute(t *testing.T) {
	f := seedRAGDocumentsApp(t)
	sharedEvidence := f.stageEvidenceObject(t, "obj_shared", f.ownerID)

	if got := f.request(http.MethodGet, "/api/v1/rag/collections/"+f.collectionID+"/documents?status=bogus", f.ownerKey); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid status = %d body=%s", got.Code, got.Body.String())
	} else if !strings.Contains(got.Body.String(), "invalid_status") {
		t.Fatalf("invalid status error code missing: %s", got.Body.String())
	}
	for _, status := range []string{"all", "ready", "processing", "failed", "removed"} {
		got := f.request(http.MethodGet, "/api/v1/rag/collections/"+f.collectionID+"/documents?status="+status, f.ownerKey)
		if got.Code != http.StatusOK {
			t.Fatalf("status %s = %d body=%s", status, got.Code, got.Body.String())
		}
		var envelope struct {
			Documents []rag.Document `json:"documents"`
			Limit     int            `json:"limit"`
			Offset    int            `json:"offset"`
		}
		if err := json.Unmarshal(got.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Limit != 20 || envelope.Offset != 0 || envelope.Documents == nil {
			t.Fatalf("status %s envelope mismatch: %#v", status, envelope)
		}
	}
	// A member without the object audience lists nothing — no titles, no
	// counts — while removed stays unknown (silently empty, not an error).
	for _, status := range []string{"all", "removed"} {
		got := f.request(http.MethodGet, "/api/v1/rag/collections/"+f.collectionID+"/documents?status="+status, f.memberKey)
		if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"documents":[]`) {
			t.Fatalf("member %s list = %d body=%s", status, got.Code, got.Body.String())
		}
	}

	// Soft delete keeps the management Removed view and drops member reads.
	if err := f.svc.DeleteDocument(context.Background(), auth.Principal{
		TenantID: f.tenant, ActorID: f.ownerID, UserID: f.ownerID, ActorKind: auth.ActorKindUser,
	}, "obj_shared"); err != nil {
		t.Fatal(err)
	}
	if got := f.request(http.MethodGet, "/api/v1/rag/collections/"+f.collectionID+"/documents?status=removed", f.ownerKey); got.Code != http.StatusOK {
		t.Fatalf("management removed list = %d body=%s", got.Code, got.Body.String())
	} else {
		var envelope struct {
			Documents []rag.Document `json:"documents"`
		}
		if err := json.Unmarshal(got.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if len(envelope.Documents) != 1 || envelope.Documents[0].Status != rag.DocumentStatusRemoved ||
			envelope.Documents[0].TextHash != "" || envelope.Documents[0].QdrantPointCount != 0 {
			t.Fatalf("removed metadata mismatch: %#v", envelope.Documents)
		}
	}
	if got := f.request(http.MethodGet, "/api/v1/rag/collections/"+f.collectionID+"/documents?status=all", f.ownerKey); strings.Contains(got.Body.String(), "obj_shared") {
		t.Fatalf("removed document leaked into the all view: %s", got.Body.String())
	}
	// The removed document's evidence is unreachable.
	if got := f.request(http.MethodGet, "/api/v1/rag/evidence/"+sharedEvidence+"/passage", f.ownerKey); got.Code != http.StatusNotFound {
		t.Fatalf("removed evidence passage = %d body=%s", got.Code, got.Body.String())
	}
}

// ---------------------------------------------------------------------
// 08-08: POST /api/v1/rag/search (bounded readable search) and the multipart
// documents upload.

// readableSearchStore is an httpx-local vector store stub returning
// pre-registered points per Qdrant collection.
type readableSearchStore struct {
	points map[string][]rag.QdrantSearchPoint
}

func newReadableSearchStore() *readableSearchStore {
	return &readableSearchStore{points: map[string][]rag.QdrantSearchPoint{}}
}

func (s *readableSearchStore) EnsureCollection(ctx context.Context, collection string, vectorSize int) error {
	return nil
}
func (s *readableSearchStore) UpsertPoints(ctx context.Context, collection string, points []rag.QdrantPoint) error {
	return nil
}
func (s *readableSearchStore) DeleteDocumentPoints(ctx context.Context, collection, documentID string) error {
	return nil
}
func (s *readableSearchStore) DeleteGenerationPoints(ctx context.Context, collection, versionID string, generation int64) error {
	return nil
}
func (s *readableSearchStore) SetPayload(ctx context.Context, collection string, filter map[string]any, payload map[string]any) error {
	return nil
}
func (s *readableSearchStore) DeleteCollection(ctx context.Context, collection string) error {
	return nil
}
func (s *readableSearchStore) SearchHybrid(ctx context.Context, collection string, vector []float64, sparse rag.SparseVector, limit int, filter map[string]any) ([]rag.QdrantSearchPoint, error) {
	return s.points[collection], nil
}
func (s *readableSearchStore) Query(ctx context.Context, collection string, in rag.QueryInput) ([]rag.QdrantSearchPoint, error) {
	points := s.points[collection]
	if len(points) > in.Limit {
		points = points[:in.Limit]
	}
	return points, nil
}

// readableSearchFixture is the 08-08 search-page fixture: real service, stub
// store, owner + member API keys and two staged public documents.
type readableSearchFixture struct {
	ragDocsFixture
	vector         *readableSearchStore
	otherID        string
	crossTenantKey string
}

func seedReadableSearchApp(t *testing.T) readableSearchFixture {
	t.Helper()
	db := testdb.New(t)
	ctx := context.Background()
	identityRepo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "rag-readable")
	owner := testdb.SeedUser(t, db, tenant, "readable-owner@example.test", nil)
	member := testdb.SeedUser(t, db, tenant, "readable-member@example.test", nil)
	ownerKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "readable-owner", OwnerUserID: owner.ID, Scopes: []string{"rag", "rag.manage"}})
	memberKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "readable-member", OwnerUserID: member.ID, Scopes: []string{"rag"}})
	otherTenant := testdb.SeedTenant(t, db, "rag-readable-other")
	otherUser := testdb.SeedUser(t, db, otherTenant, "readable-other@example.test", nil)
	otherKey, _ := testdb.SeedAPIKey(t, db, otherTenant, testdb.SeedKeySpec{Name: "readable-other", OwnerUserID: otherUser.ID, Scopes: []string{"models.list"}})
	crossTenantKey, _ := testdb.SeedAPIKey(t, db, otherTenant, testdb.SeedKeySpec{Name: "readable-other-rag", OwnerUserID: otherUser.ID, Scopes: []string{"rag"}})

	vector := newReadableSearchStore()
	svc := rag.NewService(postgres.NewRAGRepo(db), vector, nil, "", nil, nil)
	authService := auth.NewService(identityRepo, "")
	app := NewApp(authService, nil, "", nil, identityRepo, nil, nil, nil, nil, nil, nil, nil, nil, "sha256", "", "enc_", nil, nil, 1, false, nil, nil, GateConfig{}, postgres.NewSourceRepo(db), nil)
	app.ragSvc = svc
	ownerPrincipal := auth.Principal{TenantID: tenant, ActorID: owner.ID, UserID: owner.ID, ActorKind: auth.ActorKindUser}
	first, err := svc.CreateCollection(ctx, ownerPrincipal, rag.CreateCollectionInput{Name: "first", ACL: map[string]any{"public": true}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.CreateCollection(ctx, ownerPrincipal, rag.CreateCollectionInput{Name: "second", ACL: map[string]any{"public": true}})
	if err != nil {
		t.Fatal(err)
	}
	// A collection owned by another actor is unreadable to the fixture keys.
	outsiderPrincipal := auth.Principal{TenantID: tenant, ActorID: "actor_outsider", UserID: "actor_outsider", ActorKind: auth.ActorKindUser}
	secret, err := svc.CreateCollection(ctx, outsiderPrincipal, rag.CreateCollectionInput{Name: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	f := readableSearchFixture{
		ragDocsFixture: ragDocsFixture{
			app: app, db: db, repo: postgres.NewRAGRepo(db), svc: svc, tenant: tenant,
			ownerID: owner.ID, memberID: member.ID, collectionID: first.ID,
			ownerKey: ownerKey, memberKey: memberKey, otherKey: otherKey,
		},
		vector:         vector,
		otherID:        secret.ID,
		crossTenantKey: crossTenantKey,
	}
	f.request = func(method, path, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	stageReadableDoc(t, db, vector, svc, tenant, owner.ID, first, "obj_first", "readable alpha corpus", `{"kind":"upload"}`)
	stageReadableDoc(t, db, vector, svc, tenant, owner.ID, second, "obj_second", "readable beta corpus", `{"source_kind":"notion"}`)
	stageReadableDoc(t, db, vector, svc, tenant, owner.ID, secret, "obj_secret", "readable secret corpus", `{}`)
	return f
}

// stageReadableDoc persists an audience object + published generation with
// one chunk and registers its point with the stub store.
func stageReadableDoc(t *testing.T, db *postgres.DB, vector *readableSearchStore, svc *rag.Service, tenant, ownerID string, collection rag.Collection, objectID, content, provenance string) {
	t.Helper()
	ctx := context.Background()
	repo := postgres.NewRAGRepo(db)
	if _, err := repo.UpsertObject(ctx, source.Object{
		TenantID: tenant, ID: objectID, Kind: "document", NativeID: "native-" + objectID,
		AuthorizationMode: source.AuthorizationAudience, OwnerActorID: ownerID,
		Audience: []string{ownerID, "*"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.InsertObjectVersion(ctx, tenant, source.ObjectVersion{
		ObjectID: objectID, ObservedSeq: 1, ArtifactRef: "art_" + objectID, SHA256: "sha_" + objectID,
		ByteSize: int64(len(content)), MimeType: "text/plain", OriginalFilename: objectID + ".txt",
		Provenance: provenance,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddCollectionItem(ctx, tenant, collection.ID, objectID, ownerID); err != nil {
		t.Fatal(err)
	}
	parsed := source.Document{
		Ref: source.ObjectRef{TenantID: tenant, ObjectID: objectID, ObservedSeq: 1},
		Blocks: []source.Block{
			{ID: "blk_" + objectID, Ordinal: 0, Kind: "paragraph", Text: content, ByteStart: 0, ByteEnd: len(content),
				Locator: source.Locator{Page: new(2)}},
		},
	}
	if err := repo.SaveParsedDocument(ctx, "rag-structured-v1", parsed); err != nil {
		t.Fatal(err)
	}
	profileID, err := repo.EnsureChunkProfile(ctx, tenant, "rag-"+collection.ID+"-structured", 200, 20)
	if err != nil {
		t.Fatal(err)
	}
	chunkID := "chk_" + objectID
	if err := repo.ReplaceVersionChunksAndStage(ctx, tenant, collection.ID, objectID, objectID+"#1", 1, 1, profileID, []rag.DocumentChunk{{
		ID: chunkID, DocumentID: objectID, CollectionID: collection.ID, Generation: 1,
		ChunkIndex: 0, Content: content, TokenCount: 4, HeaderPath: "Section",
		SpanRefs: []source.Span{{Ref: parsed.Ref, BlockID: "blk_" + objectID, ByteStart: 0, ByteEnd: 5}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkGenerationState(ctx, tenant, collection.ID, objectID+"#1", 1, "published"); err != nil {
		t.Fatal(err)
	}
	vector.points[collection.QdrantCollection] = append(vector.points[collection.QdrantCollection], rag.QdrantSearchPoint{
		ID: "pt_" + objectID, Score: 1, Payload: map[string]any{
			"tenant_id": tenant, "document_id": objectID, "object_version_id": objectID + "#1",
			"generation": int64(1), "chunk_id": chunkID, "file_name": objectID + ".txt",
			"source_uri": "urn:test:" + objectID, "content": content, "access_enabled": true,
		},
	})
}

func searchReadable(f readableSearchFixture, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/rag/search", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, req)
	return rec
}

func TestRAGReadableSearchRoute(t *testing.T) {
	f := seedReadableSearchApp(t)

	if got := searchReadable(f, "", `{"query":"corpus"}`); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d", got.Code)
	}
	if got := searchReadable(f, f.otherKey, `{"query":"corpus"}`); got.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant key without rag scope = %d, want 403", got.Code)
	}
	// A cross-tenant caller holding `rag` is authorized but sees only its own
	// tenant's (empty) readable set — never the fixture tenant's corpus.
	foreign := searchReadable(f, f.crossTenantKey, `{"query":"corpus","mode":"lexical"}`)
	if foreign.Code != http.StatusOK || strings.Contains(foreign.Body.String(), "corpus") || strings.Contains(foreign.Body.String(), `"facets":[{`) {
		t.Fatalf("cross-tenant readable search = %d body=%s", foreign.Code, foreign.Body.String())
	}
	if got := searchReadable(f, f.ownerKey, `not-json`); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid body = %d", got.Code)
	}
	if got := searchReadable(f, f.ownerKey, `{"query":"corpus","mode":"telepathy"}`); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode = %d body=%s", got.Code, got.Body.String())
	}
	if got := searchReadable(f, f.ownerKey, `{"query":"corpus","collection_ids":["`+f.otherID+`"]}`); got.Code != http.StatusNotFound {
		t.Fatalf("explicit denied collection = %d body=%s, want 404", got.Code, got.Body.String())
	}
	// The route was previously absent (404); it now dispatches into the service.
	if got := f.request(http.MethodGet, "/api/v1/rag/search", f.ownerKey); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET on search route = %d, want 405", got.Code)
	}

	resp := searchReadable(f, f.ownerKey, `{"query":"corpus","mode":"lexical","limit":10}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("lexical readable search = %d body=%s", resp.Code, resp.Body.String())
	}
	var payload struct {
		Results []rag.SearchResult `json:"results"`
		Facets  []rag.SourceFacet  `json:"facets"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Results) != 2 {
		t.Fatalf("results=%d, want the two readable documents only: %s", len(payload.Results), resp.Body.String())
	}
	kinds := map[string]string{}
	for _, r := range payload.Results {
		kinds[r.SourceKind] = r.CollectionName
		if r.CollectionName == "" || r.SourceKind == "" || r.EvidenceID == "" {
			t.Fatalf("result missing server-side presentation fields: %#v", r)
		}
		if len(r.Locations) == 0 || r.Locations[0].Page == nil || *r.Locations[0].Page != 2 {
			t.Fatalf("locations unresolved: %#v", r)
		}
	}
	if kinds["upload"] == "" || kinds["notion"] == "" {
		t.Fatalf("source kinds must come from provenance: %#v", kinds)
	}
	if len(payload.Facets) != 2 {
		t.Fatalf("facets=%#v, want one per canonical kind", payload.Facets)
	}
	for _, facet := range payload.Facets {
		if facet.Count != 1 || (facet.SourceKind != "upload" && facet.SourceKind != "notion") {
			t.Fatalf("unexpected facet %#v", facet)
		}
	}

	// A member without any audience grant gets empty results, not leaks.
	memberView := searchReadable(f, f.memberKey, `{"query":"corpus","mode":"lexical"}`)
	if memberView.Code != http.StatusOK || strings.Contains(memberView.Body.String(), "corpus") || strings.Contains(memberView.Body.String(), `"facets":[{`) {
		t.Fatalf("member readable search = %d body=%s", memberView.Code, memberView.Body.String())
	}

	// An exceeded work bound is an explicit search_scope_too_large, never a
	// silent first-N result set.
	previous := rag.SearchWorkBudget
	defer func() { rag.SearchWorkBudget = previous }()
	rag.SearchWorkBudget = 1
	bounded := searchReadable(f, f.ownerKey, `{"query":"corpus","mode":"lexical"}`)
	if bounded.Code != http.StatusBadRequest || !strings.Contains(bounded.Body.String(), "search_scope_too_large") {
		t.Fatalf("over-bound search = %d body=%s", bounded.Code, bounded.Body.String())
	}
	rag.SearchWorkBudget = previous
	ok := searchReadable(f, f.ownerKey, `{"query":"corpus","mode":"lexical"}`)
	if ok.Code != http.StatusOK || len(ok.Body.String()) == 0 {
		t.Fatalf("restored bound search = %d", ok.Code)
	}
}

// uploadFixture is the 08-08 multipart upload fixture: real artifact store +
// jobs so the durable ingest actually runs.
type uploadFixture struct {
	app          *App
	db           *postgres.DB
	svc          *rag.Service
	store        *artifacts.Store
	jobs         *jobs.Service
	tenant       string
	collectionID string
	managerKey   string
	memberKey    string
}

func seedUploadApp(t *testing.T) uploadFixture {
	t.Helper()
	db := testdb.New(t)
	ctx := context.Background()
	identityRepo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "rag-upload")
	manager := testdb.SeedUser(t, db, tenant, "upload-manager@example.test", nil)
	member := testdb.SeedUser(t, db, tenant, "upload-member@example.test", nil)
	managerKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "upload-manager", OwnerUserID: manager.ID, Scopes: []string{"rag", "rag.manage"}})
	memberKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "upload-member", OwnerUserID: member.ID, Scopes: []string{"rag"}})
	jobsSvc := jobs.NewService(postgres.NewJobsRepo(db), 0)
	artifactRepo := postgres.NewArtifactRepo(db)
	store, err := artifacts.NewStore(t.TempDir(), t.TempDir(), artifactRepo)
	if err != nil {
		t.Fatal(err)
	}
	svc := rag.NewService(postgres.NewRAGRepo(db), nil, nil, "", store, jobsSvc)
	authService := auth.NewService(identityRepo, "")
	app := NewApp(authService, nil, "", nil, identityRepo, nil, nil, nil, nil, nil, nil, nil, nil, "sha256", "", "enc_", nil, nil, 1, false, nil, nil, GateConfig{}, postgres.NewSourceRepo(db), artifactRepo)
	app.ragSvc = svc
	collection, err := svc.CreateCollection(ctx, auth.Principal{TenantID: tenant, ActorID: manager.ID, UserID: manager.ID, ActorKind: auth.ActorKindUser},
		rag.CreateCollectionInput{Name: "uploads", ChunkSize: 200, ChunkOverlap: 20, ACL: map[string]any{"public": true}})
	if err != nil {
		t.Fatal(err)
	}
	return uploadFixture{
		app: app, db: db, svc: svc, store: store, jobs: jobsSvc, tenant: tenant,
		collectionID: collection.ID, managerKey: managerKey, memberKey: memberKey,
	}
}

func uploadMultipart(t *testing.T, f uploadFixture, key, fieldName, filename, content string, extraFields int) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for i := 0; i < extraFields; i++ {
		_ = form.WriteField(fmt.Sprintf("field_%d", i), "x")
	}
	if fieldName != "" {
		file, err := form.CreateFormFile(fieldName, filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/rag/collections/"+f.collectionID+"/documents", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, req)
	return rec
}

func TestRAGCollectionDocumentsUploadRoute(t *testing.T) {
	f := seedUploadApp(t)
	content := "uploaded durable ingest content"

	// rag alone is not enough: ingest is a billable write.
	if got := uploadMultipart(t, f, f.memberKey, "file", "member.txt", content, 0); got.Code != http.StatusForbidden {
		t.Fatalf("member upload = %d body=%s, want 403", got.Code, got.Body.String())
	}
	// Wrong field name / missing file → 400.
	if got := uploadMultipart(t, f, f.managerKey, "notfile", "wrong.txt", content, 0); got.Code != http.StatusBadRequest {
		t.Fatalf("wrong field = %d body=%s, want 400", got.Code, got.Body.String())
	}
	if got := uploadMultipart(t, f, f.managerKey, "file", "extra.txt", content, 1); got.Code != http.StatusBadRequest {
		t.Fatalf("extra fields = %d body=%s, want 400", got.Code, got.Body.String())
	}
	// Two file parts → 400.
	var double bytes.Buffer
	form := multipart.NewWriter(&double)
	for _, name := range []string{"first.txt", "second.txt"} {
		file, _ := form.CreateFormFile("file", name)
		_, _ = file.Write([]byte(content))
	}
	_ = form.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/rag/collections/"+f.collectionID+"/documents", &double)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+f.managerKey)
	if rec := httptest.NewRecorder(); func() bool { f.app.Handler().ServeHTTP(rec, req); return rec.Code != http.StatusBadRequest }() {
		t.Fatalf("two files = %d, want 400", rec.Code)
	}
	// Oversize (1 MiB app bound) → 413.
	if got := uploadMultipart(t, f, f.managerKey, "file", "big.txt", strings.Repeat("x", 2<<20), 0); got.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize upload = %d body=%s, want 413", got.Code, got.Body.String())
	}
	// Empty file → rejected after finalize with nothing left behind.
	if got := uploadMultipart(t, f, f.managerKey, "file", "empty.txt", "", 0); got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "empty") {
		t.Fatalf("empty upload = %d body=%s, want 400 emptiness", got.Code, got.Body.String())
	}
	var versions, finals int
	if err := f.db.SQL.QueryRowContext(context.Background(), `SELECT count(*) FROM source_object_versions WHERE tenant_id=$1`, f.tenant).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SQL.QueryRowContext(context.Background(), `SELECT count(*) FROM artifacts WHERE tenant_id=$1 AND state='final'`, f.tenant).Scan(&finals); err != nil {
		t.Fatal(err)
	}
	if versions != 0 || finals != 0 {
		t.Fatalf("rejected empty upload left versions=%d finals=%d", versions, finals)
	}

	// The real upload: 202 + durable ingest + real digest/size + round-trip.
	resp := uploadMultipart(t, f, f.managerKey, "file", "note.txt", content, 0)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("upload = %d body=%s, want 202", resp.Code, resp.Body.String())
	}
	var out struct {
		Document rag.Document `json:"document"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode upload output: %v body=%s", err, resp.Body.String())
	}
	if out.Document.ID == "" {
		t.Fatalf("upload output missing document: %s", resp.Body.String())
	}
	var sha256Hex string
	var byteSize int64
	if err := f.db.SQL.QueryRowContext(context.Background(),
		`SELECT content_sha256, byte_size FROM source_object_versions WHERE tenant_id=$1 AND object_id=$2`,
		f.tenant, out.Document.ID).Scan(&sha256Hex, &byteSize); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	if sha256Hex != hex.EncodeToString(sum[:]) || byteSize != int64(len(content)) {
		t.Fatalf("persisted digest/size = %s/%d, want the uploaded bytes' %s/%d", sha256Hex, byteSize, hex.EncodeToString(sum[:]), len(content))
	}
	var embeds int
	if err := f.db.SQL.QueryRowContext(context.Background(),
		`SELECT count(*) FROM jobs WHERE tenant_id=$1 AND operation='rag.embed' AND input_ref=$2`,
		f.tenant, out.Document.ID+"#1").Scan(&embeds); err != nil {
		t.Fatal(err)
	}
	if embeds != 1 {
		t.Fatalf("enqueued embed jobs = %d, want 1 referencing the real version", embeds)
	}
	// The document shows up in the authorized list and its bytes round-trip.
	list := f.app.Handler()
	got := httptest.NewRequest(http.MethodGet, "/api/v1/rag/collections/"+f.collectionID+"/documents?status=all", nil)
	got.Header.Set("Authorization", "Bearer "+f.managerKey)
	rec := httptest.NewRecorder()
	list.ServeHTTP(rec, got)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), out.Document.ID) {
		t.Fatalf("authorized list = %d body=%s", rec.Code, rec.Body.String())
	}
	download := httptest.NewRequest(http.MethodGet, "/api/v1/rag/documents/"+out.Document.ID, nil)
	download.Header.Set("Authorization", "Bearer "+f.managerKey)
	drec := httptest.NewRecorder()
	list.ServeHTTP(drec, download)
	if drec.Code != http.StatusOK || drec.Body.String() != content {
		t.Fatalf("artifact round-trip = %d body=%q", drec.Code, drec.Body.String())
	}
}
