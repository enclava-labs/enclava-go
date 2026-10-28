package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/rag"
	"enclava-go/internal/source"
	"enclava-go/internal/testdb"
)

// TestBCoverageUploadCrossTenantDenied is the B5 other-tenant row the
// single-tenant upload suite cannot express: a caller holding rag+rag.manage
// in ANOTHER tenant gets the uniform not-found on this tenant's collection —
// no collection exists, no file is stored, no job is enqueued.
func TestBCoverageUploadCrossTenantDenied(t *testing.T) {
	f := seedUploadApp(t)
	ctx := context.Background()

	foreignTenant := testdb.SeedTenant(t, f.db, "rag-upload-foreign")
	foreignUser := testdb.SeedUser(t, f.db, foreignTenant, "foreign-manager@example.test", nil)
	foreignKey, _ := testdb.SeedAPIKey(t, f.db, foreignTenant, testdb.SeedKeySpec{
		Name: "foreign-manager", OwnerUserID: foreignUser.ID, Scopes: []string{"rag", "rag.manage"},
	})

	rec := uploadMultipart(t, f, foreignKey, "file", "foreign.txt", "cross-tenant payload", 0)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant upload = %d body=%s, want the uniform 404", rec.Code, rec.Body.String())
	}
	var objects, jobRows int
	if err := f.db.SQL.QueryRowContext(ctx,
		`SELECT count(*) FROM source_objects WHERE tenant_id=$1`, f.tenant).Scan(&objects); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SQL.QueryRowContext(ctx,
		`SELECT count(*) FROM jobs WHERE tenant_id=$1`, f.tenant).Scan(&jobRows); err != nil {
		t.Fatal(err)
	}
	if objects != 0 || jobRows != 0 {
		t.Fatalf("cross-tenant upload persisted objects=%d jobs=%d", objects, jobRows)
	}
	// Unknown collection is indistinguishable from a foreign one.
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, err := form.CreateFormFile("file", "nowhere.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	_ = form.Close()
	missing := httptest.NewRequest(http.MethodPost, "/api/v1/rag/collections/ragcol_missing/documents", &body)
	missing.Header.Set("Content-Type", form.FormDataContentType())
	missing.Header.Set("Authorization", "Bearer "+f.managerKey)
	rec = httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, missing)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown collection upload = %d body=%s, want 404", rec.Code, rec.Body.String())
	}
}
func TestBCoverageDocumentListDeniedRowsNeverConsumePages(t *testing.T) {
	f := seedRAGDocumentsApp(t)

	// Two shared docs (member in audience) then three restricted docs —
	// denied rows interleave with granted ones.
	stageAudienceObject(t, f, "obj_shared_a", []string{f.ownerID, f.memberID})
	stageAudienceObject(t, f, "obj_shared_b", []string{f.ownerID, f.memberID})
	for i := range 3 {
		stageAudienceObject(t, f, fmt.Sprintf("obj_restricted_%d", i), []string{f.ownerID})
	}

	// Every page the member can request: only the two shared rows, no
	// restricted title or id, no page slot consumed.
	seen := map[string]bool{}
	for offset := 0; offset < 6; offset++ {
		path := fmt.Sprintf("/api/v1/rag/collections/%s/documents?status=all&limit=1&offset=%d", f.collectionID, offset)
		got := f.request(http.MethodGet, path, f.memberKey)
		if got.Code != http.StatusOK {
			t.Fatalf("member page offset=%d = %d body=%s", offset, got.Code, got.Body.String())
		}
		if strings.Contains(got.Body.String(), "restricted") || strings.Contains(got.Body.String(), "obj_restricted") {
			t.Fatalf("restricted row leaked at offset=%d: %s", offset, got.Body.String())
		}
		var page struct {
			Documents []struct {
				ID       string `json:"id"`
				FileName string `json:"file_name"`
			} `json:"documents"`
		}
		if err := json.Unmarshal(got.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode page: %v", err)
		}
		for _, d := range page.Documents {
			seen[d.ID] = true
		}
	}
	if len(seen) != 2 || !seen["obj_shared_a"] || !seen["obj_shared_b"] {
		t.Fatalf("member-visible docs across pages = %#v, want exactly the two shared", seen)
	}
	// A 2-per-page read covers both without a third page of denied filler.
	got := f.request(http.MethodGet,
		"/api/v1/rag/collections/"+f.collectionID+"/documents?status=all&limit=2&offset=0", f.memberKey)
	if !strings.Contains(got.Body.String(), "obj_shared_a") || !strings.Contains(got.Body.String(), "obj_shared_b") {
		t.Fatalf("first member page = %s, want both shared docs", got.Body.String())
	}
}

// stageAudienceObject persists an audience-mode object+version+generation so
// the document list projects it; audience gates who sees the row.
func stageAudienceObject(t *testing.T, f ragDocsFixture, objectID string, audience []string) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.repo.UpsertObject(ctx, source.Object{
		TenantID: f.tenant, ID: objectID, Kind: "upload", NativeID: "up_" + objectID,
		AuthorizationMode: source.AuthorizationAudience, OwnerActorID: f.ownerID,
		Audience: audience,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.InsertObjectVersion(ctx, f.tenant, source.ObjectVersion{
		ObjectID: objectID, ObservedSeq: 1, ArtifactRef: "art_" + objectID, SHA256: "sha_" + objectID,
		ByteSize: 10, MimeType: "text/plain", OriginalFilename: objectID + ".txt",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.AddCollectionItem(ctx, f.tenant, f.collectionID, objectID, f.ownerID); err != nil {
		t.Fatal(err)
	}
	// One staged, then published generation so the row projects ready.
	profileID, err := f.repo.EnsureChunkProfile(ctx, f.tenant, "rag-"+f.collectionID+"-p", 200, 20)
	if err != nil {
		t.Fatal(err)
	}
	chunks := []rag.DocumentChunk{{
		ID: "chk_" + objectID, DocumentID: objectID, CollectionID: f.collectionID,
		Generation: 1, ChunkIndex: 0, Content: objectID + " body", TokenCount: 2,
	}}
	if err := f.repo.ReplaceVersionChunksAndStage(ctx, f.tenant, f.collectionID, objectID, objectID+"#1", 1, 1, profileID, chunks); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.MarkGenerationState(ctx, f.tenant, f.collectionID, objectID+"#1", 1, "published"); err != nil {
		t.Fatal(err)
	}
}
