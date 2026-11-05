package httpx

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

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
