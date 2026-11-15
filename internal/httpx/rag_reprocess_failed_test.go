package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/jobs"
	"enclava-go/internal/rag"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

// TestRAGCollectionReprocessFailedRoute pins the bulk retry endpoint's gate
// shape: authentication, the rag.manage scope (same as the per-document
// reprocess route), method check and dispatch into the service.
func TestRAGCollectionReprocessFailedRoute(t *testing.T) {
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "bootstrap")
	user := testdb.SeedUser(t, db, tenant, "repro-failed@example.test", nil)
	manager, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "rf-mgr", OwnerUserID: user.ID, Scopes: []string{"rag", "rag.manage"}})
	reader, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "rf-read", OwnerUserID: user.ID, Scopes: []string{"rag"}})

	app := NewApp(auth.NewService(postgres.NewIdentityRepo(db), ""), nil, "", nil, postgres.NewIdentityRepo(db),
		nil, nil, nil, nil, nil, nil, nil, nil, "sha256", "", "enc_", nil, nil, 1, false, nil, nil, GateConfig{}, nil, nil)
	app.ragSvc = &rag.Service{}

	request := func(key, method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	if got := request("", http.MethodPost, "/api/v1/rag/collections/c1/reprocess-failed"); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d, want 401", got.Code)
	}
	if got := request(reader, http.MethodPost, "/api/v1/rag/collections/c1/reprocess-failed"); got.Code != http.StatusForbidden {
		t.Fatalf("reprocess-failed should require rag.manage, got %d", got.Code)
	}
	if got := request(manager, http.MethodPost, "/api/v1/rag/collections/c1/reprocess-failed"); got.Code != http.StatusInternalServerError {
		t.Fatalf("mounted route should reach the service (500 on zero Service), got %d body=%s", got.Code, got.Body.String())
	}
	if got := request(manager, http.MethodGet, "/api/v1/rag/collections/c1/reprocess-failed"); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET reprocess-failed = %d, want 405", got.Code)
	}
	app.ragSvc = nil
	if got := request(manager, http.MethodPost, "/api/v1/rag/collections/c1/reprocess-failed"); got.Code != http.StatusNotImplemented {
		t.Fatalf("unconfigured rag = %d, want 501", got.Code)
	}
}

// reprocFixture is the real pipeline fixture for the bulk endpoint: durable
// ingest + embed jobs, API-key principals (owner has rag.manage).
type reprocFixture struct {
	app      *App
	db       *postgres.DB
	svc      *rag.Service
	jobs     *jobs.Service
	tenant   string
	owner    auth.Principal
	other    auth.Principal
	ownerKey string
	otherKey string
	collID   string
	request  func(method, path, key string) *httptest.ResponseRecorder
}

func newReprocFixture(t *testing.T) *reprocFixture {
	t.Helper()
	db := testdb.New(t)
	ctx := context.Background()
	tenant := testdb.SeedTenant(t, db, "rag-repro-failed")
	ownerUser := testdb.SeedUser(t, db, tenant, "rf-owner@example.test", nil)
	otherUser := testdb.SeedUser(t, db, tenant, "rf-other@example.test", nil)
	ownerKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "rf-owner", OwnerUserID: ownerUser.ID, Scopes: []string{"rag", "rag.manage"}})
	otherKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "rf-other", OwnerUserID: otherUser.ID, Scopes: []string{"rag", "rag.manage"}})

	jobsSvc := jobs.NewService(postgres.NewJobsRepo(db), 0)
	artifactRepo := postgres.NewArtifactRepo(db)
	store, err := artifacts.NewStore(t.TempDir(), t.TempDir(), artifactRepo)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	svc := rag.NewService(postgres.NewRAGRepo(db), nil, nil, "", store, jobsSvc)

	principal := func(u auth.User, manage bool) auth.Principal {
		scopes := map[string]struct{}{"rag": {}}
		if manage {
			scopes["rag.manage"] = struct{}{}
		}
		return auth.Principal{
			TenantID: tenant, ActorID: u.ID, UserID: u.ID, ActorKind: auth.ActorKindUser,
			CredentialID: "key_test", CredentialKind: auth.CredentialKindAPIKey, Scopes: scopes,
		}
	}
	app := NewApp(auth.NewService(postgres.NewIdentityRepo(db), ""), nil, "", nil, postgres.NewIdentityRepo(db),
		nil, nil, nil, nil, nil, nil, nil, nil, "sha256", "", "enc_", nil, nil, 1, false, nil, nil, GateConfig{}, nil, nil)
	app.ragSvc = svc

	collection, err := svc.CreateCollection(ctx, principal(ownerUser, true), rag.CreateCollectionInput{
		Name: "Retry failed", ChunkSize: 200, ChunkOverlap: 20, ACL: map[string]any{"public": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &reprocFixture{
		app: app, db: db, svc: svc, jobs: jobsSvc, tenant: tenant,
		owner: principal(ownerUser, true), other: principal(otherUser, true),
		ownerKey: ownerKey, otherKey: otherKey, collID: collection.ID,
	}
	f.request = func(method, path, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	return f
}

// uploadFailedDocument runs one whitespace-only upload through the real
// pipeline and fails its embed job the way the runner would (fatal: no
// extractable text — nothing worker-related).
func (f *reprocFixture) uploadFailedDocument(t *testing.T, uploader auth.Principal, name, collectionID string) string {
	t.Helper()
	var body strings.Builder
	for _, b := range []byte(name) {
		body.WriteString(strings.Repeat(" ", int(b)+1))
		body.WriteByte('\t')
	}
	out, err := f.svc.UploadDocument(context.Background(), uploader, collectionID, rag.UploadDocumentInput{
		FileName: name, MimeType: "text/plain", Bytes: []byte(body.String()),
	})
	if err != nil {
		t.Fatalf("upload %s: %v", name, err)
	}
	for {
		claim, err := f.jobs.Lease(context.Background(), "rf-worker", time.Minute)
		if err != nil {
			t.Fatalf("lease: %v", err)
		}
		if claim == nil {
			t.Fatalf("embed job for %s never claimed", name)
		}
		if claim.Job().Operation != rag.OperationEmbed {
			if _, err := claim.Complete(context.Background(), "{}"); err != nil {
				t.Fatalf("complete bystander: %v", err)
			}
			continue
		}
		if _, herr := f.svc.JobHandler()(context.Background(), claim, nil); herr == nil {
			t.Fatalf("expected %s embed to fail", name)
		} else if err := claim.Fail(context.Background(), herr, jobs.FailureFatal); err != nil {
			t.Fatalf("settle failed job: %v", err)
		}
		return out.Document.ID
	}
}

// TestRAGCollectionReprocessFailedRequeuesFailedDocs runs the bulk endpoint
// against the real service: failed documents requeue through the same code
// path as the per-document reprocess, the response counts them, and a
// collection the caller cannot manage is the usual not-found/denied shape.
func TestRAGCollectionReprocessFailedRequeuesFailedDocs(t *testing.T) {
	f := newReprocFixture(t)
	ctx := context.Background()

	failed := map[string]bool{}
	for _, name := range []string{"failed-a.txt", "failed-b.txt"} {
		id := f.uploadFailedDocument(t, f.owner, name, f.collID)
		failed[id] = true
	}

	resp := f.request(http.MethodPost, "/api/v1/rag/collections/"+f.collID+"/reprocess-failed", f.ownerKey)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("reprocess-failed = %d, want 202 body=%s", resp.Code, resp.Body.String())
	}
	var body struct {
		Requeued int `json:"requeued"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Requeued != len(failed) {
		t.Fatalf("requeued = %d, want %d", body.Requeued, len(failed))
	}

	// Each requeued document staged a new generation and is processing again.
	docs, err := f.svc.ListDocuments(ctx, f.owner, f.collID, rag.DocumentStatusAll, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	processing := 0
	for _, d := range docs {
		if failed[d.ID] && d.Status == rag.DocumentStatusProcessing {
			processing++
		}
	}
	if processing != len(failed) {
		t.Fatalf("%d failed documents back to processing, want %d", processing, len(failed))
	}

	// Idempotent second call: nothing is failed anymore, nothing requeued.
	if resp := f.request(http.MethodPost, "/api/v1/rag/collections/"+f.collID+"/reprocess-failed", f.ownerKey); resp.Code != http.StatusAccepted {
		t.Fatalf("second call = %d, want 202", resp.Code)
	}

	// Another tenant's caller gets the collection-routes' uniform not-found.
	otherTenant := testdb.SeedTenant(t, f.db, "rag-repro-failed-other")
	otherUser := testdb.SeedUser(t, f.db, otherTenant, "rf-foreign@example.test", nil)
	foreign, _ := testdb.SeedAPIKey(t, f.db, otherTenant, testdb.SeedKeySpec{Name: "rf-foreign", OwnerUserID: otherUser.ID, Scopes: []string{"rag", "rag.manage"}})
	if rec := f.request(http.MethodPost, "/api/v1/rag/collections/"+f.collID+"/reprocess-failed", foreign); rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
		t.Fatalf("foreign reprocess-failed = %d, want 404/403", rec.Code)
	}
}
