package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/auth"
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
