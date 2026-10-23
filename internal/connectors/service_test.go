package connectors

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

type testAdapter struct {
	capabilities AdapterCapabilities
	content      func(context.Context, source.Connection, Cursor, int) (ContentPage, error)
	access       func(context.Context, source.Connection, Cursor, int) (AccessPage, error)
	membership   func(context.Context, source.Connection, Cursor, int) (MembershipPage, error)
}

func (a *testAdapter) Capabilities() AdapterCapabilities { return a.capabilities }
func (a *testAdapter) Content(ctx context.Context, c source.Connection, cursor Cursor, limit int) (ContentPage, error) {
	if a.content == nil {
		return ContentPage{}, errors.New("unexpected content request")
	}
	return a.content(ctx, c, cursor, limit)
}
func (a *testAdapter) Access(ctx context.Context, c source.Connection, cursor Cursor, limit int) (AccessPage, error) {
	if a.access == nil {
		return AccessPage{}, errors.New("unexpected access request")
	}
	return a.access(ctx, c, cursor, limit)
}
func (a *testAdapter) Membership(ctx context.Context, c source.Connection, cursor Cursor, limit int) (MembershipPage, error) {
	if a.membership == nil {
		return MembershipPage{}, errors.New("unexpected membership request")
	}
	return a.membership(ctx, c, cursor, limit)
}

type connectorFixture struct {
	db           *postgres.DB
	svc          *Service
	jobs         *jobs.Service
	jobRows      *postgres.JobsRepo
	sourceRepo   *postgres.SourceRepo
	repo         *postgres.SourceRepo
	observations *postgres.ConnectorObservationsRepo
	artifacts    *artifacts.Store
	principal    auth.Principal
	tenant       string
	connection   source.Connection
	adapter      *testAdapter
}

type fixture = connectorFixture

func newConnectorFixture(t *testing.T, adapters ...*testAdapter) *connectorFixture {
	t.Helper()
	adapter := &testAdapter{capabilities: AdapterCapabilities{Content: true}}
	if len(adapters) > 0 {
		adapter = adapters[0]
	}
	db := testdb.New(t)
	ctx := context.Background()
	tenant := testdb.SeedTenant(t, db, "connector-sync")
	user := testdb.SeedUser(t, db, tenant, "connector@example.test", nil)
	p := auth.Principal{TenantID: tenant, ActorID: user.ID, ActorKind: auth.ActorKindUser, UserID: user.ID,
		CredentialID: "key_connector", CredentialKind: auth.CredentialKindAPIKey,
		BillingSubjectID: user.ID, BillingSubjectKind: auth.BillingSubjectUser,
		Scopes: map[string]struct{}{"connectors.manage": {}},
	}
	jobsRepo := postgres.NewJobsRepo(db)
	jobsSvc := jobs.NewService(jobsRepo, 0)
	sourceRepo := postgres.NewSourceRepo(db)
	conn := source.Connection{TenantID: tenant, ID: "connection-test", Kind: source.ConnectionSlack, Name: "Slack", CredentialRef: "CONNECTOR_SLACK_TEST",
		Config: map[string]any{}, TargetCollectionID: "collection-test", Status: "active", CreatedBy: user.ID}
	if err := sourceRepo.CreateConnection(ctx, conn); err != nil {
		t.Fatal(err)
	}
	store, err := artifacts.NewStore(t.TempDir(), t.TempDir(), postgres.NewArtifactRepo(db))
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(sourceRepo, jobsSvc, jobsRepo)
	factories := AdapterFactories{}
	for _, kind := range []SourceKind{SourceKindGitHub, SourceKindSlack, SourceKindTeams, SourceKindNotion, SourceKindGoogleDrive} {
		kind := kind
		factories[kind] = func(source.Connection) (Adapter, error) { return adapter, nil }
	}
	obs := postgres.NewConnectorObservationsRepo(db)
	svc.SetSyncDependencies(factories, obs, store, sourceRepo)
	return &connectorFixture{db: db, svc: svc, jobs: jobsSvc, jobRows: jobsRepo, sourceRepo: sourceRepo, repo: sourceRepo, observations: obs,
		artifacts: store, principal: p, tenant: tenant, connection: conn, adapter: adapter}
}

func (f *connectorFixture) runNext(t *testing.T) (jobs.Job, error) {
	t.Helper()
	ctx := context.Background()
	for {
		claim, err := f.jobs.Lease(ctx, "connector-test-worker", time.Minute)
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
			return claim.Job(), err
		}
		if _, err := claim.Complete(ctx, result); err != nil {
			t.Fatal(err)
		}
		return claim.Job(), nil
	}
}

func (f *connectorFixture) schedule(t *testing.T) SyncJob {
	t.Helper()
	job, err := f.svc.SyncSource(context.Background(), f.principal, f.connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestContentSyncPersistsFinalArtifactRefAndEnqueuesIngest(t *testing.T) {
	adapter := &testAdapter{capabilities: AdapterCapabilities{Content: true}}
	adapter.content = func(context.Context, source.Connection, Cursor, int) (ContentPage, error) {
		return ContentPage{Items: []ContentItem{{NativeID: "file-1", Kind: "document", Filename: "note.txt", MIMEType: "text/plain", Content: []byte("durable connector content"), URL: "https://example.test/note"}}, Completeness: CompletenessComplete, Snapshot: true}, nil
	}
	f := newConnectorFixture(t, adapter)
	f.schedule(t)
	if job, err := f.runNext(t); err != nil || job.Operation != OperationSync {
		t.Fatalf("sync: job=%+v err=%v", job, err)
	}
	state, err := f.observations.GetStream(context.Background(), f.connection.TenantID, f.connection.ID, string(StreamContent))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.observations.ListObservations(context.Background(), state)
	if err != nil || len(rows) != 1 {
		t.Fatalf("observations=%d err=%v", len(rows), err)
	}
	row := rows[0]
	if row.ObjectRef.TenantID != f.connection.TenantID || row.ObjectRef.ObjectID == "" || row.ObjectRef.ObservedSeq != 1 {
		t.Fatalf("missing canonical source ref: %+v", row.ObjectRef)
	}
	if row.ArtifactRef.ID == "" || row.ArtifactRef.Kind != artifacts.KindOriginal || row.ArtifactRef.ByteSize != int64(len("durable connector content")) {
		t.Fatalf("missing finalized artifact ref: %+v", row.ArtifactRef)
	}
	meta, err := postgres.NewArtifactRepo(f.db).GetArtifact(context.Background(), f.connection.TenantID, row.ArtifactRef.ID)
	if err != nil || meta.State != artifacts.StateFinal {
		t.Fatalf("artifact was not finalized: %+v err=%v", meta, err)
	}
	queued, err := f.jobRows.ListConnectionJobs(context.Background(), f.connection.TenantID, f.connection.ID, jobs.StatusQueued, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range queued {
		found = found || item.Operation == OperationIngest && item.Subject.ObservedRevision == row.ObjectRef.ObservedSeq
	}
	if !found {
		t.Fatalf("rag.ingest not durably queued: %+v", queued)
	}
}

func TestPermissionOnlySyncAppliesCanonicalAccessWithoutContentIngest(t *testing.T) {
	adapter := &testAdapter{capabilities: AdapterCapabilities{Access: true}}
	adapter.access = func(context.Context, source.Connection, Cursor, int) (AccessPage, error) {
		return AccessPage{Grants: []AccessGrant{{NativeObjectID: "doc-1", NativeObjectKind: "document", NativePrincipalID: "native-user-1", Scope: "read", Effect: source.GrantEffectAllow}}, Completeness: CompletenessComplete, Snapshot: true}, nil
	}
	f := newConnectorFixture(t, adapter)
	object, err := f.sourceRepo.UpsertObject(context.Background(), source.Object{TenantID: f.connection.TenantID, ConnectionID: f.connection.ID, ID: "object-doc-1", Kind: "document", NativeID: "doc-1", AuthorizationMode: source.AuthorizationSourceACL, ObservedSeq: 1})
	if err != nil {
		t.Fatal(err)
	}
	user := testdb.SeedUser(t, f.db, f.connection.TenantID, "mapped@example.test", nil)
	if err := f.sourceRepo.InsertSourceIdentity(context.Background(), source.SourceIdentity{TenantID: f.connection.TenantID, ConnectionID: f.connection.ID, SourceKind: f.connection.Kind, ID: "identity-native-user-1", NativeID: "native-user-1", MappedUserID: user.ID}); err != nil {
		t.Fatal(err)
	}
	f.schedule(t)
	if _, err := f.runNext(t); err != nil {
		t.Fatal(err)
	}
	revision, _, err := f.sourceRepo.LatestCompleteRevision(context.Background(), f.connection.TenantID, f.connection.ID, source.SnapshotGrants)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := f.sourceRepo.GrantsForObject(context.Background(), f.connection.TenantID, object.ID, revision)
	if err != nil || len(grants) != 1 || grants[0].SubjectID != "identity-native-user-1" {
		t.Fatalf("canonical grants=%+v err=%v", grants, err)
	}
	queued, err := f.jobRows.ListConnectionJobs(context.Background(), f.connection.TenantID, f.connection.ID, jobs.StatusQueued, 20)
	if err != nil {
		t.Fatal(err)
	}
	foundAccess, foundIngest := false, false
	for _, item := range queued {
		foundAccess = foundAccess || item.Operation == OperationAccess
		foundIngest = foundIngest || item.Operation == OperationIngest
	}
	if !foundAccess || foundIngest {
		t.Fatalf("permission-only handoff mismatch: %+v", queued)
	}
}

func TestCanonicalAccessSnapshotsFollowAtoBtoA(t *testing.T) {
	grants := []string{source.GrantEffectAllow, source.GrantEffectDeny, source.GrantEffectAllow}
	call := 0
	adapter := &testAdapter{capabilities: AdapterCapabilities{Access: true}}
	adapter.access = func(context.Context, source.Connection, Cursor, int) (AccessPage, error) {
		effect := grants[call]
		call++
		return AccessPage{Grants: []AccessGrant{{NativeObjectID: "doc-1", NativeObjectKind: "document", NativePrincipalID: "native-user-1", Scope: "read", Effect: effect}}, Completeness: CompletenessComplete, Snapshot: true}, nil
	}
	f := newConnectorFixture(t, adapter)
	object, err := f.sourceRepo.UpsertObject(context.Background(), source.Object{TenantID: f.connection.TenantID, ConnectionID: f.connection.ID, ID: "object-doc-1", Kind: "document", NativeID: "doc-1", AuthorizationMode: source.AuthorizationSourceACL, ObservedSeq: 1})
	if err != nil {
		t.Fatal(err)
	}
	user := testdb.SeedUser(t, f.db, f.connection.TenantID, "mapped@example.test", nil)
	if err := f.sourceRepo.InsertSourceIdentity(context.Background(), source.SourceIdentity{TenantID: f.connection.TenantID, ConnectionID: f.connection.ID, SourceKind: f.connection.Kind, ID: "identity-native-user-1", NativeID: "native-user-1", MappedUserID: user.ID}); err != nil {
		t.Fatal(err)
	}
	for i, effect := range grants {
		if _, err := f.svc.SyncSourceWindowed(context.Background(), f.principal, f.connection.ID, int64(i+1)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.runNext(t); err != nil {
			t.Fatal(err)
		}
		revision, _, err := f.sourceRepo.LatestCompleteRevision(context.Background(), f.connection.TenantID, f.connection.ID, source.SnapshotGrants)
		if err != nil {
			t.Fatal(err)
		}
		current, err := f.sourceRepo.GrantsForObject(context.Background(), f.connection.TenantID, object.ID, revision)
		if err != nil || len(current) != 1 || current[0].Effect != effect {
			t.Fatalf("snapshot %d effect=%q grants=%+v err=%v", i, effect, current, err)
		}
	}
}

func TestIncompleteContentListingDoesNotDeleteUnseenObjects(t *testing.T) {
	adapter := &testAdapter{capabilities: AdapterCapabilities{Content: true}}
	adapter.content = func(context.Context, source.Connection, Cursor, int) (ContentPage, error) {
		return ContentPage{Completeness: CompletenessIncomplete, Snapshot: true}, nil
	}
	f := newConnectorFixture(t, adapter)
	old, err := f.sourceRepo.UpsertObject(context.Background(), source.Object{TenantID: f.connection.TenantID, ConnectionID: f.connection.ID, ID: "object-old", Kind: "document", NativeID: "old", AuthorizationMode: source.AuthorizationSourceACL, ObservedSeq: 1})
	if err != nil {
		t.Fatal(err)
	}
	f.schedule(t)
	if _, err := f.runNext(t); err == nil {
		t.Fatal("incomplete listing unexpectedly succeeded")
	}
	stored, err := f.sourceRepo.GetObject(context.Background(), f.connection.TenantID, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DeletedAt != nil {
		t.Fatal("incomplete listing deleted unseen object")
	}
}

func TestSyncSourceEnqueuesIndependentSupportedStreams(t *testing.T) {
	adapter := &testAdapter{capabilities: AdapterCapabilities{Content: true, Access: true}}
	adapter.content = func(context.Context, source.Connection, Cursor, int) (ContentPage, error) {
		return ContentPage{Completeness: CompletenessComplete}, nil
	}
	adapter.access = func(context.Context, source.Connection, Cursor, int) (AccessPage, error) {
		return AccessPage{Completeness: CompletenessIncomplete}, nil
	}
	f := newConnectorFixture(t, adapter)
	f.schedule(t)
	rows, err := f.jobRows.ListConnectionJobs(context.Background(), f.connection.TenantID, f.connection.ID, jobs.StatusQueued, 20)
	if err != nil {
		t.Fatal(err)
	}
	streams := map[string]bool{}
	for _, row := range rows {
		if row.Operation == OperationSync {
			streams[stringFromAny(row.Policy["stream"])] = true
		}
	}
	if !streams[string(StreamContent)] || !streams[string(StreamAccess)] {
		t.Fatalf("missing independently queued stream jobs: %v", streams)
	}
}

func TestCreateSourceStillRejectsUnsafeCredentialsAndPrivateOrigins(t *testing.T) {
	adapter := &testAdapter{capabilities: AdapterCapabilities{Content: true}}
	f := newConnectorFixture(t, adapter)
	f.svc.setAllowPrivateBaseURL(false)
	_, err := f.svc.CreateSource(context.Background(), f.principal, CreateSourceInput{Type: SourceTypeSlack, Name: "bad credential", CredentialRef: "CONNECTOR_GITHUB_TEST"})
	if err == nil || !strings.Contains(err.Error(), "credential_ref") {
		t.Fatalf("unsafe credential ref accepted: %v", err)
	}
	_, err = f.svc.CreateSource(context.Background(), f.principal, CreateSourceInput{Type: SourceTypeSlack, Name: "private origin", CredentialRef: "CONNECTOR_SLACK_TEST", Config: map[string]any{"base_url": "http://127.0.0.1:9000"}})
	if err == nil {
		t.Fatal("private base URL accepted")
	}
}

func TestRequireAPIOriginEnforcesHTTPSExceptLoopback(t *testing.T) {
	for _, ok := range []string{"https://api.example.com", "https://api.example.com/v1", "http://127.0.0.1:9", "http://localhost:9", "http://[::1]:9"} {
		if _, err := requireAPIOrigin(ok); err != nil {
			t.Errorf("requireAPIOrigin(%q) = %v, want accepted", ok, err)
		}
	}
	for _, bad := range []string{"http://api.example.com", "ftp://api.example.com", "https://user@api.example.com", "https://api.example.com?q=1", "", "https://"} {
		if _, err := requireAPIOrigin(bad); err == nil {
			t.Errorf("requireAPIOrigin(%q) accepted, want rejected", bad)
		}
	}
}
