package connectors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	grants, err := f.sourceRepo.GrantsForObjects(context.Background(), f.connection.TenantID, f.connection.ID, []string{object.ID}, revision)
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
		current, err := f.sourceRepo.GrantsForObjects(context.Background(), f.connection.TenantID, f.connection.ID, []string{object.ID}, revision)
		if err != nil || len(current) != 1 || current[0].Effect != effect {
			t.Fatalf("snapshot %d effect=%q grants=%+v err=%v", i, effect, current, err)
		}
	}
}

func TestSlackParentGrantBeforeContentAndIdentityBinding(t *testing.T) {
	ctx := context.Background()
	adapter := &testAdapter{capabilities: AdapterCapabilities{Access: true}}
	adapter.access = func(context.Context, source.Connection, Cursor, int) (AccessPage, error) {
		return AccessPage{Grants: []AccessGrant{
			{NativeObjectID: "slack:C123", NativeObjectKind: "channel", NativePrincipalID: "U-mapped", NativeGroupID: "C123", Scope: "read", Effect: source.GrantEffectAllow},
			{NativeObjectID: "slack:C123", NativeObjectKind: "channel", NativePrincipalID: "U-unmapped", NativeGroupID: "C123", Scope: "read", Effect: source.GrantEffectAllow},
		}, Completeness: CompletenessComplete, Snapshot: true}, nil
	}
	f := newConnectorFixture(t, adapter)
	user := testdb.SeedUser(t, f.db, f.tenant, "slack-mapped@example.test", nil)
	if err := f.sourceRepo.InsertSourceIdentity(ctx, source.SourceIdentity{TenantID: f.tenant, ConnectionID: f.connection.ID, SourceKind: f.connection.Kind, ID: "mapped-slack", NativeID: "U-mapped", MappedUserID: user.ID}); err != nil {
		t.Fatal(err)
	}
	if err := f.sourceRepo.InsertSourceIdentity(ctx, source.SourceIdentity{TenantID: f.tenant, ConnectionID: f.connection.ID, SourceKind: f.connection.Kind, ID: "unmapped-slack", NativeID: "U-unmapped"}); err != nil {
		t.Fatal(err)
	}
	f.schedule(t)
	if _, err := f.runNext(t); err != nil {
		t.Fatal(err)
	}
	parent, err := f.sourceRepo.ObjectForNativeID(ctx, f.tenant, f.connection.ID, "channel", "slack:C123")
	if err != nil {
		t.Fatalf("grant did not create channel stub: %v", err)
	}
	if _, err := f.sourceRepo.SourceIdentityForNativeID(ctx, f.tenant, f.connection.ID, "U-unmapped"); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("unmapped source identity resolved as authorized: %v", err)
	}
	if err := f.sourceRepo.BeginSnapshot(ctx, f.tenant, f.connection.ID, source.SnapshotMembership, 1); err != nil {
		t.Fatal(err)
	}
	if err := f.sourceRepo.WithTx(ctx, func(tx source.Tx) error {
		return f.sourceRepo.ApplyMembershipSnapshot(ctx, tx, f.tenant, f.connection.ID, 1, nil, 0)
	}); err != nil {
		t.Fatal(err)
	}
	adapter.capabilities = AdapterCapabilities{Content: true}
	adapter.content = func(context.Context, source.Connection, Cursor, int) (ContentPage, error) {
		return ContentPage{Items: []ContentItem{
			{NativeID: "slack:C123", Kind: "channel", Text: "channel"},
			{NativeID: "slack:C123:thread-1", ParentNativeID: "slack:C123", ParentKind: "channel", Kind: "message", Text: "thread message"},
			{NativeID: "slack:C123:reply-1", ParentNativeID: "slack:C123:thread-1", ParentKind: "message", Kind: "message", Text: "thread reply"},
		}, Completeness: CompletenessComplete}, nil
	}
	if _, err := f.svc.SyncSourceWindowed(ctx, f.principal, f.connection.ID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runNext(t); err != nil {
		t.Fatal(err)
	}
	observedParent, err := f.sourceRepo.ObjectForNativeID(ctx, f.tenant, f.connection.ID, "channel", "slack:C123")
	if err != nil || observedParent.ID != parent.ID {
		t.Fatalf("channel stub was not reused: %+v err=%v", observedParent, err)
	}
	child, err := f.sourceRepo.ObjectForNativeID(ctx, f.tenant, f.connection.ID, "message", "slack:C123:thread-1")
	if err != nil || child.ParentObjectID != parent.ID {
		t.Fatalf("thread parent = %+v, err=%v", child, err)
	}
	reply, err := f.sourceRepo.ObjectForNativeID(ctx, f.tenant, f.connection.ID, "message", "slack:C123:reply-1")
	if err != nil || reply.ParentObjectID != child.ID {
		t.Fatalf("reply parent = %+v, err=%v", reply, err)
	}
	authorizer := source.NewAuthorizer(f.sourceRepo, 0)
	mapped := f.principal
	mapped.UserID, mapped.ActorID = user.ID, user.ID
	if err := authorizer.CanRead(ctx, mapped, child, source.ReadOptions{}); err != nil {
		t.Fatalf("mapped channel member cannot read thread: %v", err)
	}
	if err := authorizer.CanRead(ctx, mapped, reply, source.ReadOptions{}); err != nil {
		t.Fatalf("mapped channel member cannot read nested reply: %v", err)
	}
	other := testdb.SeedUser(t, f.db, f.tenant, "slack-unmapped@example.test", nil)
	unbound := f.principal
	unbound.UserID, unbound.ActorID = other.ID, other.ID
	if err := authorizer.CanRead(ctx, unbound, child, source.ReadOptions{}); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("unmapped user read thread: %v", err)
	}
	if err := authorizer.CanRead(ctx, unbound, reply, source.ReadOptions{}); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("unmapped user read nested reply: %v", err)
	}
	admin := f.principal
	admin.Scopes[auth.ScopePlatformAdmin] = struct{}{}
	if _, err := f.svc.BindSourcePrincipal(ctx, admin, BindSourcePrincipalInput{ConnectionID: f.connection.ID, NativeUserID: "U-unmapped", MappedUserID: other.ID, VerificationEvidence: "verified in Slack"}); err != nil {
		t.Fatal(err)
	}
	adapter.capabilities = AdapterCapabilities{Access: true}
	if _, err := f.svc.SyncSourceWindowed(ctx, f.principal, f.connection.ID, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runNext(t); err != nil {
		t.Fatal(err)
	}
	if err := authorizer.CanRead(ctx, unbound, child, source.ReadOptions{}); err != nil {
		t.Fatalf("newly mapped channel member cannot read thread: %v", err)
	}
	if err := authorizer.CanRead(ctx, unbound, reply, source.ReadOptions{}); err != nil {
		t.Fatalf("newly mapped channel member cannot read nested reply: %v", err)
	}
}

func TestSubjectlessDenyOverridesDirectAllow(t *testing.T) {
	ctx := context.Background()
	adapter := &testAdapter{capabilities: AdapterCapabilities{Access: true}}
	adapter.access = func(context.Context, source.Connection, Cursor, int) (AccessPage, error) {
		return AccessPage{Grants: []AccessGrant{
			{NativeObjectID: "doc-1", NativeObjectKind: "document", NativePrincipalID: "native-user", Scope: "read", Effect: source.GrantEffectAllow},
			{NativeObjectID: "doc-1", NativeObjectKind: "document", Scope: "provider-restriction", Effect: source.GrantEffectDeny},
		}, Completeness: CompletenessComplete, Snapshot: true}, nil
	}
	f := newConnectorFixture(t, adapter)
	user := testdb.SeedUser(t, f.db, f.tenant, "deny-test@example.test", nil)
	if err := f.sourceRepo.InsertSourceIdentity(ctx, source.SourceIdentity{TenantID: f.tenant, ConnectionID: f.connection.ID, SourceKind: f.connection.Kind, ID: "deny-test-identity", NativeID: "native-user", MappedUserID: user.ID}); err != nil {
		t.Fatal(err)
	}
	f.schedule(t)
	if _, err := f.runNext(t); err != nil {
		t.Fatal(err)
	}
	object, err := f.sourceRepo.ObjectForNativeID(ctx, f.tenant, f.connection.ID, "document", "doc-1")
	if err != nil {
		t.Fatal(err)
	}
	revision, _, err := f.sourceRepo.LatestCompleteRevision(ctx, f.tenant, f.connection.ID, source.SnapshotGrants)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := f.sourceRepo.GrantsForObjects(ctx, f.tenant, f.connection.ID, []string{object.ID}, revision)
	if err != nil {
		t.Fatal(err)
	}
	foundDeny, foundAllow := false, false
	for _, grant := range grants {
		foundDeny = foundDeny || grant.SubjectKind == source.GrantSubjectAll && grant.SubjectID == "" && grant.Effect == source.GrantEffectDeny
		foundAllow = foundAllow || grant.SubjectID == "deny-test-identity" && grant.Effect == source.GrantEffectAllow
	}
	if !foundDeny || !foundAllow {
		t.Fatalf("missing persisted all-subject deny or direct allow: %+v", grants)
	}
	principal := f.principal
	principal.ActorID, principal.UserID = user.ID, user.ID
	if err := source.NewAuthorizer(f.sourceRepo, 0).CanRead(ctx, principal, object, source.ReadOptions{}); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("subjectless deny did not override direct allow: %v", err)
	}
}

func TestContentDedupPreservesAtoBtoA(t *testing.T) {
	ctx := context.Background()
	value := "A"
	adapter := &testAdapter{capabilities: AdapterCapabilities{Content: true}}
	adapter.content = func(context.Context, source.Connection, Cursor, int) (ContentPage, error) {
		return ContentPage{Items: []ContentItem{{NativeID: "C123:thread-1", Kind: "thread", Filename: "thread.txt", MIMEType: "text/plain", Text: value}}, Completeness: CompletenessComplete}, nil
	}
	f := newConnectorFixture(t, adapter)
	for i, step := range []struct {
		content string
		seq     int64
	}{{"A", 1}, {"A", 1}, {"B", 2}, {"A", 3}} {
		value = step.content
		if _, err := f.svc.SyncSourceWindowed(ctx, f.principal, f.connection.ID, int64(i+1)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.runNext(t); err != nil {
			t.Fatal(err)
		}
		object, err := f.sourceRepo.ObjectForNativeID(ctx, f.tenant, f.connection.ID, "thread", "C123:thread-1")
		if err != nil {
			t.Fatal(err)
		}
		version, err := f.sourceRepo.LatestObjectVersion(ctx, f.tenant, object.ID)
		if err != nil || version.ObservedSeq != step.seq {
			t.Fatalf("step %d: version=%+v err=%v", i, version, err)
		}
		var count int64
		if err := f.db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE tenant_id=$1 AND operation=$2`, f.tenant, OperationIngest).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != step.seq {
			t.Fatalf("step %d: got %d ingest jobs, want %d", i, count, step.seq)
		}
	}
}

func TestContentResyncReEnqueuesStrandedVersion(t *testing.T) {
	ctx := context.Background()
	adapter := &testAdapter{capabilities: AdapterCapabilities{Content: true}}
	adapter.content = func(context.Context, source.Connection, Cursor, int) (ContentPage, error) {
		return ContentPage{Items: []ContentItem{{NativeID: "file-1", Kind: "document", Filename: "note.txt", MIMEType: "text/plain", Content: []byte("identical bytes")}}, Completeness: CompletenessComplete, Snapshot: true}, nil
	}
	f := newConnectorFixture(t, adapter)
	f.schedule(t)
	if _, err := f.runNext(t); err != nil {
		t.Fatal(err)
	}
	object, err := f.sourceRepo.ObjectForNativeID(ctx, f.tenant, f.connection.ID, "document", "file-1")
	if err != nil {
		t.Fatal(err)
	}
	version, err := f.sourceRepo.LatestObjectVersion(ctx, f.tenant, object.ID)
	if err != nil || version.ObservedSeq != 1 {
		t.Fatalf("version=%+v err=%v", version, err)
	}
	// Simulate a crash after the version row committed but before its ingest
	// job durably existed: the version+artifact stay, the job is gone.
	if _, err := f.db.SQL.ExecContext(ctx, `DELETE FROM jobs WHERE tenant_id=$1 AND operation=$2`, f.tenant, OperationIngest); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SyncSourceWindowed(ctx, f.principal, f.connection.ID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runNext(t); err != nil {
		t.Fatal(err)
	}
	queued, err := f.jobRows.ListConnectionJobs(ctx, f.tenant, f.connection.ID, jobs.StatusQueued, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range queued {
		if item.Operation == OperationIngest && item.Subject.ObjectID == object.ID && item.Subject.ObservedRevision == version.ObservedSeq {
			found = true
		}
	}
	if !found {
		t.Fatalf("stranded version was not re-enqueued for ingest: %+v", queued)
	}
	var versions int64
	if err := f.db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM source_object_versions WHERE tenant_id=$1 AND object_id=$2`, f.tenant, object.ID).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Fatalf("identical bytes must not create a new version row, got %d", versions)
	}
}

func TestContentMetadataChangeNewVersionReusesArtifact(t *testing.T) {
	ctx := context.Background()
	title, filename, url := "Original Title", "original.txt", "https://example.test/original"
	adapter := &testAdapter{capabilities: AdapterCapabilities{Content: true}}
	adapter.content = func(context.Context, source.Connection, Cursor, int) (ContentPage, error) {
		return ContentPage{Items: []ContentItem{{NativeID: "file-1", Kind: "document", Title: title, Filename: filename, MIMEType: "text/plain", URL: url, Content: []byte("identical bytes")}}, Completeness: CompletenessComplete, Snapshot: true}, nil
	}
	f := newConnectorFixture(t, adapter)
	f.schedule(t)
	if _, err := f.runNext(t); err != nil {
		t.Fatal(err)
	}
	object, err := f.sourceRepo.ObjectForNativeID(ctx, f.tenant, f.connection.ID, "document", "file-1")
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.sourceRepo.LatestObjectVersion(ctx, f.tenant, object.ID)
	if err != nil {
		t.Fatal(err)
	}
	title, filename, url = "Renamed Title", "renamed.txt", "https://example.test/renamed"
	if _, err := f.svc.SyncSourceWindowed(ctx, f.principal, f.connection.ID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runNext(t); err != nil {
		t.Fatal(err)
	}
	latest, err := f.sourceRepo.LatestObjectVersion(ctx, f.tenant, object.ID)
	if err != nil || latest.ObservedSeq != 2 {
		t.Fatalf("metadata change must allocate a new observed seq: %+v err=%v", latest, err)
	}
	if latest.ArtifactRef != first.ArtifactRef || latest.SHA256 != first.SHA256 {
		t.Fatalf("unchanged bytes must reuse the artifact: first=%+v latest=%+v", first, latest)
	}
	if latest.OriginalFilename != filename || provenanceValue(latest.Provenance, "title") != title || provenanceValue(latest.Provenance, "source_uri") != url {
		t.Fatalf("new version did not carry updated provenance: %+v", latest)
	}
	var artifacts int64
	if err := f.db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM artifacts WHERE tenant_id=$1`, f.tenant).Scan(&artifacts); err != nil {
		t.Fatal(err)
	}
	if artifacts != 1 {
		t.Fatalf("metadata-only change must not create a new artifact, got %d", artifacts)
	}
	queued, err := f.jobRows.ListConnectionJobs(ctx, f.tenant, f.connection.ID, jobs.StatusQueued, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range queued {
		if item.Operation == OperationIngest && item.Subject.ObservedRevision == 2 && item.InputRef == first.ArtifactRef {
			found = true
		}
	}
	if !found {
		t.Fatalf("new version ingest job not queued: %+v", queued)
	}
}

func TestConfigurationChangeResetsIncompleteCursor(t *testing.T) {
	ctx := context.Background()
	adapter := &testAdapter{capabilities: AdapterCapabilities{Content: true}}
	adapter.content = func(_ context.Context, _ source.Connection, cursor Cursor, _ int) (ContentPage, error) {
		if cursor == "" {
			return ContentPage{NextCursor: "old-cursor", Completeness: CompletenessIncomplete}, nil
		}
		return ContentPage{}, errors.New("interrupted")
	}
	f := newConnectorFixture(t, adapter)
	f.schedule(t)
	if _, err := f.runNext(t); err == nil {
		t.Fatal("expected interrupted sync")
	}
	state, err := f.observations.GetStream(ctx, f.tenant, f.connection.ID, string(StreamContent))
	if err != nil || state.Cursor != "old-cursor" {
		t.Fatalf("expected persisted cursor: %+v err=%v", state, err)
	}
	if _, err := f.svc.UpdateSource(ctx, f.principal, f.connection.ID, UpdateSourceInput{Config: map[string]any{"channel": "C-new"}}); err != nil {
		t.Fatal(err)
	}
	adapter.content = func(_ context.Context, _ source.Connection, cursor Cursor, _ int) (ContentPage, error) {
		if cursor != "" {
			t.Fatalf("old configuration cursor reused: %q", cursor)
		}
		return ContentPage{Completeness: CompletenessComplete}, nil
	}
	if _, err := f.svc.SyncSourceWindowed(ctx, f.principal, f.connection.ID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runNext(t); err != nil {
		t.Fatal(err)
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

func TestConnectorHTTPClientRefusesPrivateDial(t *testing.T) {
	client := newConnectorHTTPClient()
	// Loopback must dial (hermetic fixtures).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	if _, err := client.Get(srv.URL); err != nil {
		t.Fatalf("loopback dial refused: %v", err)
	}
	// A private-range resolved address must be refused at dial time.
	req, err := http.NewRequest(http.MethodGet, "http://192.0.2.1:1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Transport.RoundTrip(req); err == nil {
		t.Fatal("private-address dial accepted, want refusal")
	}
}

// TestContentSyncRewriteAndRetargetHandoff covers two regressions: a content
// change alongside a metadata change must persist the new bytes instead of
// reusing the stale artifact, and retargeting the connector to a different
// collection must enqueue a fresh ingest handoff rather than treating the
// old collection's job as durable.
func TestContentSyncRewriteAndRetargetHandoff(t *testing.T) {
	ctx := context.Background()
	item := ContentItem{NativeID: "file-1", Kind: "document", Filename: "note.txt", MIMEType: "text/plain", Title: "note", URL: "https://example.test/note", Content: []byte("old body")}
	adapter := &testAdapter{capabilities: AdapterCapabilities{Content: true}}
	adapter.content = func(context.Context, source.Connection, Cursor, int) (ContentPage, error) {
		return ContentPage{Items: []ContentItem{item}, Completeness: CompletenessComplete, Snapshot: true}, nil
	}
	f := newConnectorFixture(t, adapter)
	sync := func(window int64) {
		t.Helper()
		if _, err := f.svc.SyncSourceWindowed(ctx, f.principal, f.connection.ID, window); err != nil {
			t.Fatal(err)
		}
		if _, err := f.runNext(t); err != nil {
			t.Fatal(err)
		}
	}
	latest := func() source.ObjectVersion {
		t.Helper()
		object, err := f.sourceRepo.ObjectForNativeID(ctx, f.tenant, f.connection.ID, "document", "file-1")
		if err != nil {
			t.Fatal(err)
		}
		version, err := f.sourceRepo.LatestObjectVersion(ctx, f.tenant, object.ID)
		if err != nil {
			t.Fatal(err)
		}
		return version
	}
	digest := func(body string) string {
		sum := sha256.Sum256([]byte(body))
		return hex.EncodeToString(sum[:])
	}
	drainIngests := func() map[string]bool {
		t.Helper()
		seen := map[string]bool{}
		for {
			claim, err := f.jobs.Lease(ctx, "ingest-drain", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if claim == nil {
				return seen
			}
			job := claim.Job()
			if job.Operation != OperationIngest {
				t.Fatalf("unexpected queued job %q", job.Operation)
			}
			collection, _ := job.Policy["collection_id"].(string)
			seen[collection] = true
			if _, err := claim.Complete(ctx, "{}"); err != nil {
				t.Fatal(err)
			}
		}
	}
	readBody := func(artifactID string) string {
		t.Helper()
		reader, err := f.artifacts.Read(ctx, artifacts.Ref{TenantID: f.tenant, ID: artifactID, Kind: artifacts.KindOriginal, Version: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = reader.Close() }()
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	sync(1)
	v1 := latest()
	if v1.ArtifactRef == "" || v1.SHA256 != digest("old body") {
		t.Fatalf("initial version mismatch: %+v", v1)
	}
	if !drainIngests()[f.connection.TargetCollectionID] {
		t.Fatalf("initial ingest not enqueued for %s", f.connection.TargetCollectionID)
	}

	// Content and metadata both change: the new bytes must be stored, not the
	// stale artifact reused.
	item.Title, item.URL, item.Content = "note v2", "https://example.test/note-v2", []byte("new body")
	sync(2)
	v2 := latest()
	if v2.ObservedSeq != v1.ObservedSeq+1 || v2.ArtifactRef == v1.ArtifactRef || v2.SHA256 != digest("new body") {
		t.Fatalf("changed content did not produce a new artifact version: %+v", v2)
	}
	if body := readBody(v2.ArtifactRef); body != "new body" {
		t.Fatalf("persisted artifact body %q, want %q", body, "new body")
	}
	drainIngests()

	// Metadata-only change still reuses the artifact.
	item.Title = "note v3"
	sync(3)
	v3 := latest()
	if v3.ArtifactRef != v2.ArtifactRef || v3.SHA256 != digest("new body") {
		t.Fatalf("metadata-only change must reuse artifact: %+v", v3)
	}
	drainIngests()

	// Retargeting enqueues ingest for the new collection despite the old
	// collection's completed handoff.
	conn, err := f.sourceRepo.GetConnection(ctx, f.tenant, f.connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	conn.TargetCollectionID = "collection-b"
	if err := f.sourceRepo.UpdateConnection(ctx, conn); err != nil {
		t.Fatal(err)
	}
	sync(4)
	if seen := drainIngests(); !seen["collection-b"] {
		t.Fatalf("retargeted ingest not enqueued for collection-b: %v", seen)
	}

	// Re-sync to the same target stays idempotent.
	sync(5)
	if seen := drainIngests(); len(seen) != 0 {
		t.Fatalf("same-collection re-sync enqueued ingest: %v", seen)
	}
}
