package connectors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

// notionSyncFixture wires the real NotionAdapter (against an httptest API)
// into the durable connector sync pipeline used by service_test.go.
type notionSyncFixture struct {
	db           *postgres.DB
	svc          *Service
	jobsSvc      *jobs.Service
	sourceRepo   *postgres.SourceRepo
	observations *postgres.ConnectorObservationsRepo
	principal    auth.Principal
	tenant       string
	conn         source.Connection
}

func newNotionSyncFixture(t *testing.T, handler http.Handler) *notionSyncFixture {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Setenv("NOTION_SYNC_FIXTURE_TOKEN", "fixture-token")

	db := testdb.New(t)
	ctx := context.Background()
	tenant := testdb.SeedTenant(t, db, "notion-sync")
	user := testdb.SeedUserWithGrants(t, db, tenant, "notion@example.test", []string{"connectors.manage"}, auth.ResourceGrants{
		Models: auth.Allowlist{All: true}, Endpoints: auth.Allowlist{All: true},
	})
	p := auth.Principal{TenantID: tenant, ActorID: user.ID, ActorKind: auth.ActorKindUser, UserID: user.ID,
		CredentialID: "key_notion", CredentialKind: auth.CredentialKindAPIKey,
		BillingSubjectID: user.ID, BillingSubjectKind: auth.BillingSubjectUser,
		Scopes: map[string]struct{}{"connectors.manage": {}},
	}
	resolver, err := auth.NewSecretResolver([]auth.SecretBinding{{
		RefID: "CONNECTOR_NOTION_TEST", TenantID: tenant, Purpose: auth.SecretPurposeConnector,
		Origins: []string{server.URL}, EnvName: "NOTION_SYNC_FIXTURE_TOKEN",
	}})
	if err != nil {
		t.Fatal(err)
	}
	conn := source.Connection{TenantID: tenant, ID: "conn-notion-test", Kind: source.ConnectionNotion, Name: "Notion",
		CredentialRef: "CONNECTOR_NOTION_TEST", TargetCollectionID: "collection-test",
		Config: map[string]any{
			"base_url": server.URL + "/v1", "max_depth": float64(8), "max_pages": float64(100),
			"selected_pages": []any{map[string]any{"page_id": "root"}},
		},
		Status: "active", CreatedBy: user.ID,
	}
	sourceRepo := postgres.NewSourceRepo(db)
	if err := sourceRepo.CreateConnection(ctx, conn); err != nil {
		t.Fatal(err)
	}
	store, err := artifacts.NewStore(t.TempDir(), t.TempDir(), postgres.NewArtifactRepo(db))
	if err != nil {
		t.Fatal(err)
	}
	jobsRepo := postgres.NewJobsRepo(db)
	jobsSvc := jobs.NewService(jobsRepo, 0)
	svc := NewService(sourceRepo, jobsSvc, jobsRepo)
	svc.setAllowPrivateBaseURL(true)
	factories := AdapterFactories{
		SourceKindNotion: func(c source.Connection) (Adapter, error) { return NewNotionAdapter(c, resolver) },
	}
	obs := postgres.NewConnectorObservationsRepo(db)
	svc.SetSyncDependencies(factories, obs, store, sourceRepo)
	svc.SetPrincipalResolver(auth.NewService(postgres.NewIdentityRepo(db), "").PrincipalForActor)
	return &notionSyncFixture{db: db, svc: svc, jobsSvc: jobsSvc, sourceRepo: sourceRepo, observations: obs,
		principal: p, tenant: tenant, conn: conn}
}

// runSync enqueues and executes one connector.sync job, returning the sync
// error (nil on success). Non-sync jobs fanned out by the sync are completed.
func (f *notionSyncFixture) runSync(t *testing.T) error {
	t.Helper()
	ctx := context.Background()
	if _, err := f.svc.SyncSource(ctx, f.principal, f.conn.ID); err != nil {
		t.Fatal(err)
	}
	for {
		claim, err := f.jobsSvc.Lease(ctx, "notion-test-worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if claim == nil {
			t.Fatal("expected queued connector job")
		}
		if claim.Job().Operation != OperationSync {
			if _, err := claim.Complete(ctx, "{}"); err != nil {
				t.Fatal(err)
			}
			continue
		}
		result, err := f.svc.JobHandler()(ctx, claim, nil)
		if err != nil {
			_ = claim.Fail(ctx, err, jobs.FailureFatal)
			return err
		}
		if _, err := claim.Complete(ctx, result); err != nil {
			t.Fatal(err)
		}
		return nil
	}
}

// seedLegacyObject writes the rows the pre-v1.2 adapter produced: one object
// per page (kind "page") and one per block (kind "block", raw JSON content),
// each with an immutable version row.
func (f *notionSyncFixture) seedLegacyObject(t *testing.T, kind, nativeID, parentID string) source.Object {
	t.Helper()
	ctx := context.Background()
	obj, err := f.sourceRepo.UpsertObject(ctx, source.Object{
		TenantID: f.tenant, ConnectionID: f.conn.ID, ID: "obj-" + kind + "-" + nativeID,
		Kind: kind, NativeID: nativeID, ParentObjectID: parentID,
		AuthorizationMode: source.AuthorizationCollection, ObservedSeq: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("legacy " + kind + " " + nativeID))
	revision := "2026-01-01T00:00:00Z"
	if err := f.sourceRepo.InsertObjectVersion(ctx, f.tenant, source.ObjectVersion{
		ObjectID: obj.ID, ObservedSeq: 1, ArtifactRef: "legacy-artifact-" + nativeID,
		SHA256: hex.EncodeToString(digest[:]), ByteSize: 64,
		MimeType: "application/json", OriginalFilename: nativeID + ".json",
		UpstreamRevision: &revision,
		Provenance:       `{"connection_id":"` + f.conn.ID + `","source_kind":"notion","native_id":"` + nativeID + `"}`,
	}); err != nil {
		t.Fatal(err)
	}
	return obj
}

func notionSyncAPI(t *testing.T, children http.HandlerFunc) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/pages/root", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(notionPageJSON("root", "Root Page", "https://www.notion.so/root-page"))
	})
	mux.HandleFunc("/v1/users/user-1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "user-1", "name": "Ada", "type": "person"})
	})
	mux.HandleFunc("/v1/users/user-9", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "user-9", "name": "Grace", "type": "person"})
	})
	mux.HandleFunc("/v1/blocks/root/children", children)
	return mux
}

func liveNotionChildren(w http.ResponseWriter, r *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{
		notionBlockJSON("blk-live-1", "paragraph", map[string]any{"rich_text": notionPlain("live paragraph")}),
		notionBlockJSON("blk-live-2", "heading_1", map[string]any{"rich_text": notionPlain("Live Heading")}),
	}, "has_more": false})
}

func objectByNative(t *testing.T, f *notionSyncFixture, kind, nativeID string) source.Object {
	t.Helper()
	obj, err := f.sourceRepo.ObjectForNativeID(context.Background(), f.tenant, f.conn.ID, kind, nativeID)
	if err != nil {
		t.Fatalf("object %s/%s: %v", kind, nativeID, err)
	}
	return obj
}

// TestNotionSyncTombstonesLegacyBlockItems is the upgrade path: an instance
// that synced with the old per-block adapter has page+block objects; the
// first complete sync with page documents must tombstone every old block
// object while the page object survives with a new version.
func TestNotionSyncTombstonesLegacyBlockItems(t *testing.T) {
	f := newNotionSyncFixture(t, notionSyncAPI(t, liveNotionChildren))
	ctx := context.Background()

	page := f.seedLegacyObject(t, "page", "root", "")
	live := f.seedLegacyObject(t, "block", "blk-live-1", page.ID)
	gone := f.seedLegacyObject(t, "block", "blk-gone", page.ID)

	if err := f.runSync(t); err != nil {
		t.Fatalf("complete sync failed: %v", err)
	}

	pageAfter, err := f.sourceRepo.GetObject(ctx, f.tenant, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pageAfter.DeletedAt != nil {
		t.Fatal("page document was tombstoned")
	}
	latest, err := f.sourceRepo.LatestObjectVersion(ctx, f.tenant, page.ID)
	if err != nil || latest.ObservedSeq != 2 {
		t.Fatalf("page version = %+v err=%v, want new seq 2", latest, err)
	}
	for _, stale := range []source.Object{live, gone} {
		stored, err := f.sourceRepo.GetObject(ctx, f.tenant, stale.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.DeletedAt == nil {
			t.Fatalf("legacy block object %s (%s) was not tombstoned", stale.NativeID, stale.Kind)
		}
	}

	// The page document is the only live object and carries structured blocks.
	objects, err := f.sourceRepo.ListObjectsByConnection(ctx, f.tenant, f.conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	liveCount := 0
	for _, obj := range objects {
		if obj.DeletedAt == nil {
			liveCount++
			if obj.Kind != "page" || obj.NativeID != "root" {
				t.Fatalf("unexpected live object: %+v", obj)
			}
		}
	}
	if liveCount != 1 {
		t.Fatalf("live objects = %d, want 1 page document", liveCount)
	}
}

// TestNotionIncompleteSyncKeepsLegacyBlockItems: when the traversal fails,
// the sync is incomplete and the completeness rule must not authorize any
// deletion — old block objects stay untouched.
func TestNotionIncompleteSyncKeepsLegacyBlockItems(t *testing.T) {
	f := newNotionSyncFixture(t, notionSyncAPI(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream boom", http.StatusInternalServerError)
	}))
	ctx := context.Background()

	page := f.seedLegacyObject(t, "page", "root", "")
	live := f.seedLegacyObject(t, "block", "blk-live-1", page.ID)

	if err := f.runSync(t); err == nil {
		t.Fatal("expected sync to fail on upstream error")
	}
	for _, obj := range []source.Object{page, live} {
		stored, err := f.sourceRepo.GetObject(ctx, f.tenant, obj.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.DeletedAt != nil {
			t.Fatalf("incomplete sync tombstoned %s/%s", stored.Kind, stored.NativeID)
		}
	}
}
