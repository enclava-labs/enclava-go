package postgres

import (
	"context"
	"errors"
	"testing"

	"enclava-go/internal/source"
)

func seedUser(t *testing.T, ctx context.Context, db *DB, tenant, id string) {
	t.Helper()
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO users (tenant_id, id, email)
		VALUES ($1, $2, $3)`,
		tenant, id, id+"@test.local"); err != nil {
		t.Fatalf("seed user %s: %v", id, err)
	}
}

func newRepo(t *testing.T) (*DB, *SourceRepo, context.Context) {
	t.Helper()
	db := RequireTestDB(t)
	return db, NewSourceRepo(db), context.Background()
}

// TestApplyAccessSnapshot proves a COMPLETE grants snapshot retires unseen
// grants while an incomplete revision removes nothing.
func TestApplyAccessSnapshot(t *testing.T) {
	db, repo, ctx := newRepo(t)
	seedTenant(t, ctx, db, idTenantA)
	seedUser(t, ctx, db, idTenantA, "user1")
	if err := repo.CreateConnection(ctx, source.Connection{
		TenantID: idTenantA, ID: "conn_a", Kind: "github", Name: "gh", CredentialRef: "ref"}); err != nil {
		t.Fatalf("create connection: %v", err)
	}
	if err := repo.InsertSourceIdentity(ctx, source.SourceIdentity{
		TenantID: idTenantA, ConnectionID: "conn_a", SourceKind: "github",
		ID: "sid_u1", NativeID: "u1", MappedUserID: "user1"}); err != nil {
		t.Fatalf("insert identity: %v", err)
	}
	if _, err := repo.UpsertObject(ctx, source.Object{
		TenantID: idTenantA, ConnectionID: "conn_a", ID: "obj_a", Kind: "repo",
		NativeID: "acme/widgets", AuthorizationMode: source.AuthorizationSourceACL,
		ObservedSeq: 1}); err != nil {
		t.Fatalf("upsert object: %v", err)
	}

	grant := func(objID, subjectKind, subjectID, effect string, rev int64) source.SourceGrant {
		return source.SourceGrant{TenantID: idTenantA, ConnectionID: "conn_a",
			ObjectID: objID, SubjectKind: subjectKind, SubjectID: subjectID,
			Effect: effect, AccessRevision: rev}
	}
	countGrants := func() (allow, deny int) {
		if err := db.SQL.QueryRowContext(ctx, `
			SELECT COUNT(*) FILTER (WHERE effect='allow'),
			       COUNT(*) FILTER (WHERE effect='deny')
			FROM source_grants WHERE tenant_id = $1`, idTenantA).Scan(&allow, &deny); err != nil {
			t.Fatalf("count grants: %v", err)
		}
		return
	}

	// Revision 1 completes with two grants.
	if err := repo.BeginSnapshot(ctx, idTenantA, "conn_a", source.SnapshotGrants, 1); err != nil {
		t.Fatalf("begin snapshot 1: %v", err)
	}
	if err := repo.WithTx(ctx, func(tx source.Tx) error {
		return repo.ApplyAccessSnapshot(ctx, tx, idTenantA, "conn_a", 1, []source.SourceGrant{
			grant("obj_a", "user", "sid_u1", "allow", 1),
			grant("obj_a", "user", "sid_u1", "deny", 1),
		}, 3)
	}); err != nil {
		t.Fatalf("apply snapshot 1: %v", err)
	}
	allow, deny := countGrants()
	if allow != 1 || deny != 1 {
		t.Fatalf("expected 1 allow + 1 deny at rev1, got %d/%d", allow, deny)
	}

	// Revision 2 completes without the deny — the unseen deny is retired.
	if err := repo.BeginSnapshot(ctx, idTenantA, "conn_a", source.SnapshotGrants, 2); err != nil {
		t.Fatalf("begin snapshot 2: %v", err)
	}
	if err := repo.WithTx(ctx, func(tx source.Tx) error {
		return repo.ApplyAccessSnapshot(ctx, tx, idTenantA, "conn_a", 2, []source.SourceGrant{
			grant("obj_a", "user", "sid_u1", "allow", 2),
		}, 1)
	}); err != nil {
		t.Fatalf("apply snapshot 2: %v", err)
	}
	allow, deny = countGrants()
	if allow != 2 || deny != 0 {
		t.Fatalf("expected rev1+rev2 allows and deny pruned, got allow=%d deny=%d", allow, deny)
	}
	var unmapped int
	if err := db.SQL.QueryRowContext(ctx, `SELECT unmapped_subjects FROM access_snapshots
		WHERE tenant_id = $1 AND connection_id = 'conn_a' AND kind = 'grants' AND revision = 2`, idTenantA).Scan(&unmapped); err != nil || unmapped != 1 {
		t.Fatalf("grant unmapped count=%d err=%v", unmapped, err)
	}
	grants, err := repo.GrantsForObjects(ctx, idTenantA, "conn_a", []string{"obj_a", "missing"}, 2)
	if err != nil || len(grants) != 1 || grants[0].SubjectID != "sid_u1" || grants[0].Effect != "allow" {
		t.Fatalf("current grants=%+v err=%v", grants, err)
	}
	if _, err := repo.GrantsForObjects(ctx, idTenantA, "conn_a", []string{"obj_a"}, 1); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("superseded grant revision: %v", err)
	}
	grants, err = repo.GrantsForObjects(ctx, idTenantA, "conn_a", []string{"missing"}, 2)
	if err != nil || len(grants) != 0 {
		t.Fatalf("empty grants=%+v err=%v", grants, err)
	}

	// Revision 3 never completes: begin + insert rows via a rolled-back
	// apply leaves everything intact.
	if err := repo.BeginSnapshot(ctx, idTenantA, "conn_a", source.SnapshotGrants, 3); err != nil {
		t.Fatalf("begin snapshot 3: %v", err)
	}
	before, _ := countGrants()
	err = repo.WithTx(ctx, func(tx source.Tx) error {
		if err := repo.ApplyAccessSnapshot(ctx, tx, idTenantA, "conn_a", 3, nil, 7); err != nil {
			return err
		}
		return errors.New("abort") // caller abandons the observation
	})
	if err == nil {
		t.Fatalf("expected abort error")
	}
	after, _ := countGrants()
	if after != before {
		t.Fatalf("aborted snapshot changed grants: %d -> %d", before, after)
	}

	// The incomplete revision is distinguishable and not authoritative.
	var status string
	if err := db.SQL.QueryRowContext(ctx, `
		SELECT status, unmapped_subjects FROM access_snapshots
		WHERE tenant_id = $1 AND connection_id = 'conn_a' AND kind = 'grants' AND revision = 3`,
		idTenantA).Scan(&status, &unmapped); err != nil {
		t.Fatalf("read snapshot 3: %v", err)
	}
	if status != "incomplete" || unmapped != 0 {
		t.Fatalf("snapshot 3 status=%q unmapped=%d, want incomplete and zero", status, unmapped)
	}
	if err := repo.BeginSnapshot(ctx, idTenantA, "conn_a", source.SnapshotGrants, 4); err != nil {
		t.Fatalf("begin all-subject snapshot: %v", err)
	}
	if err := repo.WithTx(ctx, func(tx source.Tx) error {
		return repo.ApplyAccessSnapshot(ctx, tx, idTenantA, "conn_a", 4, []source.SourceGrant{
			grant("obj_a", source.GrantSubjectAll, "", source.GrantEffectDeny, 4),
		}, 0)
	}); err != nil {
		t.Fatalf("apply all-subject snapshot: %v", err)
	}
	grants, err = repo.GrantsForObjects(ctx, idTenantA, "conn_a", []string{"obj_a"}, 4)
	if err != nil || len(grants) != 1 || grants[0].SubjectKind != source.GrantSubjectAll || grants[0].SubjectID != "" || grants[0].Effect != source.GrantEffectDeny {
		t.Fatalf("all-subject grant=%+v err=%v", grants, err)
	}
}

// TestApplyMembershipSnapshot proves atomic membership replacement at a
// complete revision and that an incomplete replacement never displaces the
// last complete set.
func TestApplyMembershipSnapshot(t *testing.T) {
	db, repo, ctx := newRepo(t)
	seedTenant(t, ctx, db, idTenantA)
	seedUser(t, ctx, db, idTenantA, "user1")
	seedUser(t, ctx, db, idTenantA, "user2")
	if err := repo.CreateConnection(ctx, source.Connection{
		TenantID: idTenantA, ID: "conn_a", Kind: "github", Name: "gh", CredentialRef: "ref"}); err != nil {
		t.Fatalf("create connection: %v", err)
	}
	for _, id := range []struct{ id, native, user string }{
		{"sid_u1", "u1", "user1"},
		{"sid_u2", "u2", "user2"},
	} {
		if err := repo.InsertSourceIdentity(ctx, source.SourceIdentity{
			TenantID: idTenantA, ConnectionID: "conn_a", SourceKind: "github",
			ID: id.id, NativeID: id.native, MappedUserID: id.user}); err != nil {
			t.Fatalf("insert identity %s: %v", id.id, err)
		}
	}
	if err := repo.InsertSourceGroup(ctx, source.SourceGroup{
		TenantID: idTenantA, ConnectionID: "conn_a", SourceKind: "github",
		ID: "grp_1", NativeID: "g1", Name: "devs"}); err != nil {
		t.Fatalf("insert group: %v", err)
	}

	member := func(memberID string, rev int64) source.SourceGroupMembership {
		return source.SourceGroupMembership{TenantID: idTenantA, ConnectionID: "conn_a",
			GroupID: "grp_1", MemberID: memberID, AccessRevision: rev}
	}

	// Complete revision 1: sid_u1 in grp_1.
	if err := repo.BeginSnapshot(ctx, idTenantA, "conn_a", source.SnapshotMembership, 1); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := repo.WithTx(ctx, func(tx source.Tx) error {
		return repo.ApplyMembershipSnapshot(ctx, tx, idTenantA, "conn_a", 1, []source.SourceGroupMembership{member("sid_u1", 1)}, 2)
	}); err != nil {
		t.Fatalf("apply rev1: %v", err)
	}

	// An incomplete revision 2 row must not displace the rev-1 set.
	if err := repo.BeginSnapshot(ctx, idTenantA, "conn_a", source.SnapshotMembership, 2); err != nil {
		t.Fatalf("begin rev2: %v", err)
	}
	if err := repo.InsertSourceMembership(ctx, member("sid_u2", 2)); err != nil {
		t.Fatalf("insert incomplete membership: %v", err)
	}
	groups, err := repo.GroupIDsForIdentity(ctx, idTenantA, "conn_a", "sid_u1", 1)
	if err != nil {
		t.Fatalf("groups for sid_u1: %v", err)
	}
	if len(groups) != 1 || groups[0] != "grp_1" {
		t.Fatalf("expected sid_u1 still in grp_1 at latest complete rev, got %v", groups)
	}
	groups, err = repo.GroupIDsForIdentity(ctx, idTenantA, "conn_a", "sid_u2", 1)
	if err != nil {
		t.Fatalf("groups for sid_u2: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("incomplete rev2 membership must not be authoritative, got %v", groups)
	}

	// Completing revision 2 replaces the set atomically.
	if err := repo.WithTx(ctx, func(tx source.Tx) error {
		return repo.ApplyMembershipSnapshot(ctx, tx, idTenantA, "conn_a", 2, []source.SourceGroupMembership{member("sid_u2", 2)}, 4)
	}); err != nil {
		t.Fatalf("apply rev2: %v", err)
	}
	groups, err = repo.GroupIDsForIdentity(ctx, idTenantA, "conn_a", "sid_u1", 2)
	if err != nil {
		t.Fatalf("groups for sid_u1 post-rev2: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("sid_u1 membership should be retired, got %v", groups)
	}
	groups, err = repo.GroupIDsForIdentity(ctx, idTenantA, "conn_a", "sid_u2", 2)
	if err != nil {
		t.Fatalf("groups for sid_u2 post-rev2: %v", err)
	}
	if len(groups) != 1 || groups[0] != "grp_1" {
		t.Fatalf("sid_u2 should be in grp_1, got %v", groups)
	}
	if _, err := repo.GroupIDsForIdentity(ctx, idTenantA, "conn_a", "sid_u2", 1); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("superseded membership revision: %v", err)
	}
	var unmapped int
	if err := db.SQL.QueryRowContext(ctx, `SELECT unmapped_subjects FROM access_snapshots
		WHERE tenant_id = $1 AND connection_id = 'conn_a' AND kind = 'membership' AND revision = 2`, idTenantA).Scan(&unmapped); err != nil || unmapped != 4 {
		t.Fatalf("membership unmapped count=%d err=%v", unmapped, err)
	}
}

// TestSourceRepoObjects covers upsert dedupe, tombstone visibility, parent
// hierarchy and cross-tenant rejection.
func TestSourceRepoObjects(t *testing.T) {
	db, repo, ctx := newRepo(t)
	seedTenant(t, ctx, db, idTenantA)
	seedTenant(t, ctx, db, idTenantB)
	if err := repo.CreateConnection(ctx, source.Connection{
		TenantID: idTenantA, ID: "conn_a", Kind: "github", Name: "gh", CredentialRef: "ref"}); err != nil {
		t.Fatalf("create connection: %v", err)
	}

	obj, err := repo.UpsertObject(ctx, source.Object{
		TenantID: idTenantA, ConnectionID: "conn_a", ID: "obj_parent", Kind: "repo",
		NativeID: "acme/widgets", AuthorizationMode: source.AuthorizationSourceACL,
		ObservedSeq: 1})
	if err != nil {
		t.Fatalf("upsert parent: %v", err)
	}
	if obj.ID != "obj_parent" {
		t.Fatalf("upsert returned %q", obj.ID)
	}
	// Same identity returns the persisted row — identity fields unmutated.
	again, err := repo.UpsertObject(ctx, source.Object{
		TenantID: idTenantA, ConnectionID: "conn_a", ID: "obj_parent_dup", Kind: "repo",
		NativeID: "acme/widgets", AuthorizationMode: source.AuthorizationOwner,
		OwnerActorID: "user1", ObservedSeq: 5})
	if err != nil {
		t.Fatalf("upsert duplicate: %v", err)
	}
	if again.ID != "obj_parent" || again.AuthorizationMode != source.AuthorizationSourceACL {
		t.Fatalf("upsert mutated identity: %+v", again)
	}

	// Hierarchy works.
	child, err := repo.UpsertObject(ctx, source.Object{
		TenantID: idTenantA, ConnectionID: "conn_a", ID: "obj_child", Kind: "file",
		NativeID: "acme/widgets/main.go", ParentObjectID: "obj_parent",
		AuthorizationMode: source.AuthorizationSourceACL, ObservedSeq: 2})
	if err != nil {
		t.Fatalf("upsert child: %v", err)
	}
	if child.ParentObjectID != "obj_parent" {
		t.Fatalf("child parent = %q", child.ParentObjectID)
	}
	ids, err := repo.AncestorObjectIDs(ctx, idTenantA, "conn_a", "obj_child", 1)
	if err != nil || len(ids) != 2 || ids[0] != "obj_child" || ids[1] != "obj_parent" {
		t.Fatalf("ancestor chain=%v err=%v", ids, err)
	}
	if _, err := repo.AncestorObjectIDs(ctx, idTenantA, "conn_a", "obj_child", 0); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("depth limit: %v", err)
	}
	if _, err := repo.AncestorObjectIDs(ctx, idTenantB, "conn_a", "obj_child", 8); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("cross-tenant chain: %v", err)
	}
	if _, err := repo.AncestorObjectIDs(ctx, idTenantA, "conn_a", "missing", 8); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("missing object: %v", err)
	}
	if err := repo.ObserveObject(ctx, idTenantA, "obj_parent", "obj_child", nil); err != nil {
		t.Fatalf("create cycle: %v", err)
	}
	if _, err := repo.AncestorObjectIDs(ctx, idTenantA, "conn_a", "obj_child", 8); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("cyclic chain: %v", err)
	}
	if err := repo.ObserveObject(ctx, idTenantA, "obj_parent", "", nil); err != nil {
		t.Fatalf("restore parent: %v", err)
	}

	// Cross-tenant parent insert fails on the composite FK.
	if _, err := repo.UpsertObject(ctx, source.Object{
		TenantID: idTenantB, ConnectionID: "", ID: "obj_evil", Kind: "file",
		NativeID: "x", ParentObjectID: "obj_parent",
		AuthorizationMode: source.AuthorizationOwner, ObservedSeq: 1}); err == nil {
		t.Fatalf("cross-tenant parent insert was accepted")
	}

	// Versions: monotonic insert, cross-tenant rejected.
	ver := source.ObjectVersion{ObjectID: "obj_parent", ObservedSeq: 1,
		ArtifactRef: "art1", SHA256: "aa", ByteSize: 10, MimeType: "text/plain"}
	if err := repo.InsertObjectVersion(ctx, idTenantA, ver); err != nil {
		t.Fatalf("insert version: %v", err)
	}
	if err := repo.InsertObjectVersion(ctx, idTenantA, ver); err == nil {
		t.Fatalf("non-monotonic observed_seq was accepted")
	}
	if err := repo.InsertObjectVersion(ctx, idTenantB, ver); err == nil {
		t.Fatalf("cross-tenant version insert was accepted")
	}
	latest, err := repo.LatestObjectVersion(ctx, idTenantA, "obj_parent")
	if err != nil {
		t.Fatalf("latest version: %v", err)
	}
	if latest.ObservedSeq != 1 || latest.ArtifactRef != "art1" {
		t.Fatalf("latest version = %+v", latest)
	}

	// Tombstoned object stays queryable but flagged.
	if err := repo.TombstoneObject(ctx, idTenantA, "obj_child"); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	got, err := repo.GetObject(ctx, idTenantA, "obj_child")
	if err != nil {
		t.Fatalf("get tombstoned object: %v", err)
	}
	if got.TombstonedAt == nil {
		t.Fatalf("tombstone flag missing")
	}
	if _, err := repo.AncestorObjectIDs(ctx, idTenantA, "conn_a", "obj_child", 8); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("tombstoned chain: %v", err)
	}
	revision := "new-revision"
	if err := repo.ObserveObject(ctx, idTenantA, "obj_child", "obj_parent", &revision); err != nil {
		t.Fatalf("observe tombstoned object: %v", err)
	}
	got, err = repo.GetObject(ctx, idTenantA, "obj_child")
	if err != nil || got.TombstonedAt != nil || got.UpstreamRevision == nil || *got.UpstreamRevision != revision {
		t.Fatalf("observed object=%+v err=%v", got, err)
	}
}

// TestSourceRepoRestrictions proves deny-capability lists and required
// assurance round-trip per subject kind.
func TestSourceRepoRestrictions(t *testing.T) {
	db, repo, ctx := newRepo(t)
	seedTenant(t, ctx, db, idTenantA)
	if err := repo.CreateConnection(ctx, source.Connection{
		TenantID: idTenantA, ID: "conn_a", Kind: "github", Name: "gh", CredentialRef: "ref"}); err != nil {
		t.Fatalf("create connection: %v", err)
	}

	want := source.SourceRestriction{
		TenantID: idTenantA, SubjectKind: source.RestrictionConnection, SubjectID: "conn_a",
		RequiredAssurance: "high", DeniedCapabilities: []string{"public_endpoint", "egress"},
		PolicyLabels: []string{"confidential"},
	}
	if err := repo.SetRestriction(ctx, want); err != nil {
		t.Fatalf("set restriction: %v", err)
	}
	got, err := repo.RestrictionsForSubject(ctx, idTenantA, source.RestrictionConnection, "conn_a")
	if err != nil {
		t.Fatalf("get restrictions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 restriction, got %v", got)
	}
	r := got[0]
	if r.RequiredAssurance != "high" || len(r.DeniedCapabilities) != 2 || len(r.PolicyLabels) != 1 {
		t.Fatalf("restriction round-trip mismatch: %+v", r)
	}
}

// TestIngestHandoffExists proves the crash-window query: a matching jobs row
// or an index generation for the observed object version both count as
// durable handoff; unrelated rows do not.
func TestIngestHandoffExists(t *testing.T) {
	db, repo, ctx := newRepo(t)
	seedTenant(t, ctx, db, idTenantA)
	if err := repo.CreateConnection(ctx, source.Connection{
		TenantID: idTenantA, ID: "conn_a", Kind: "github", Name: "gh", CredentialRef: "ref"}); err != nil {
		t.Fatalf("create connection: %v", err)
	}
	if _, err := repo.UpsertObject(ctx, source.Object{
		TenantID: idTenantA, ConnectionID: "conn_a", ID: "obj_a", Kind: "repo",
		NativeID: "acme/widgets", AuthorizationMode: source.AuthorizationSourceACL,
		ObservedSeq: 1}); err != nil {
		t.Fatalf("upsert object: %v", err)
	}
	if err := repo.InsertObjectVersion(ctx, idTenantA, source.ObjectVersion{
		ObjectID: "obj_a", ObservedSeq: 5, ArtifactRef: "art_a",
		SHA256: "aa", ByteSize: 1, MimeType: "text/plain"}); err != nil {
		t.Fatalf("insert version: %v", err)
	}
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO knowledge_collections (tenant_id, id, name, qdrant_collection)
		VALUES ($1, 'col_a', 'Col A', 'qdr_a')`, idTenantA); err != nil {
		t.Fatalf("insert collection: %v", err)
	}

	assert := func(want bool, note string) {
		got, err := repo.IngestHandoffExists(ctx, idTenantA, "conn_a", "col_a", "obj_a", 5)
		if err != nil || got != want {
			t.Fatalf("%s: handoff=%v err=%v, want %v", note, got, err, want)
		}
	}
	assert(false, "no jobs or generations")

	// A job covering a different observed seq is not handoff for seq 5.
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO jobs (tenant_id, id, operation, status, connection_id,
		                  object_id, observed_revision, idempotency_key,
		                  input_hash, principal, submitter_key, policy)
		VALUES ($1, 'job_other', 'rag.ingest', 'queued', 'conn_a',
		        'obj_a', 4, 'key-other', 'h', '{}', 'sub',
		        '{"collection_id":"col_a"}'::jsonb)`, idTenantA); err != nil {
		t.Fatalf("insert unrelated job: %v", err)
	}
	assert(false, "job for other seq")

	// Terminated jobs (failed/cancelled/deleted) never count as handoff.
	for _, st := range []string{"failed", "cancelled", "deleted"} {
		if _, err := db.SQL.ExecContext(ctx, `
			INSERT INTO jobs (tenant_id, id, operation, status, connection_id,
			                  object_id, observed_revision, idempotency_key,
			                  input_hash, principal, submitter_key, policy)
			VALUES ($1, $2, 'rag.ingest', $3, 'conn_a',
			        'obj_a', 5, $4, 'h', '{}', 'sub',
			        '{"collection_id":"col_a"}'::jsonb)`,
			idTenantA, "job_dead_"+st, st, "key-dead-"+st); err != nil {
			t.Fatalf("insert %s job: %v", st, err)
		}
	}
	assert(false, "dead job statuses")

	// A job covering exactly (connection, object, seq, collection) is handoff.
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO jobs (tenant_id, id, operation, status, connection_id,
		                  object_id, observed_revision, idempotency_key,
		                  input_hash, principal, submitter_key, policy)
		VALUES ($1, 'job_seq5', 'rag.ingest', 'queued', 'conn_a',
		        'obj_a', 5, 'key-seq5', 'h', '{}', 'sub',
		        '{"collection_id":"col_a"}'::jsonb)`, idTenantA); err != nil {
		t.Fatalf("insert matching job: %v", err)
	}
	assert(true, "job covers seq 5")

	// A job targeting a different collection is not handoff for col_b.
	if got, err := repo.IngestHandoffExists(ctx, idTenantA, "conn_a", "col_b", "obj_a", 5); err != nil || got {
		t.Fatalf("job for col_a must not count for col_b: handoff=%v err=%v", got, err)
	}
}

// TestIngestHandoffExistsGeneration proves the generation half: a generation
// for the object version in the target collection is durable handoff even
// with no jobs row, and only in that collection.
func TestIngestHandoffExistsGeneration(t *testing.T) {
	db, repo, ctx := newRepo(t)
	seedTenant(t, ctx, db, idTenantA)
	if err := repo.CreateConnection(ctx, source.Connection{
		TenantID: idTenantA, ID: "conn_a", Kind: "github", Name: "gh", CredentialRef: "ref"}); err != nil {
		t.Fatalf("create connection: %v", err)
	}
	if _, err := repo.UpsertObject(ctx, source.Object{
		TenantID: idTenantA, ConnectionID: "conn_a", ID: "obj_a", Kind: "repo",
		NativeID: "acme/widgets", AuthorizationMode: source.AuthorizationSourceACL,
		ObservedSeq: 1}); err != nil {
		t.Fatalf("upsert object: %v", err)
	}
	if err := repo.InsertObjectVersion(ctx, idTenantA, source.ObjectVersion{
		ObjectID: "obj_a", ObservedSeq: 3, ArtifactRef: "art_a",
		SHA256: "aa", ByteSize: 1, MimeType: "text/plain"}); err != nil {
		t.Fatalf("insert version: %v", err)
	}
	for _, col := range []string{"col_a", "col_b"} {
		if _, err := db.SQL.ExecContext(ctx, `
			INSERT INTO knowledge_collections (tenant_id, id, name, qdrant_collection)
			VALUES ($1, $2, $2, 'qdr_' || $2)`, idTenantA, col); err != nil {
			t.Fatalf("insert collection %s: %v", col, err)
		}
	}
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO index_generations (tenant_id, id, collection_id,
		                               object_version_id, generation, state)
		VALUES ($1, 'gen_a', 'col_a', 'obj_a#3', 1, 'published')`, idTenantA); err != nil {
		t.Fatalf("insert generation: %v", err)
	}

	exists, err := repo.IngestHandoffExists(ctx, idTenantA, "conn_a", "col_a", "obj_a", 3)
	if err != nil || !exists {
		t.Fatalf("generation in target collection: handoff=%v err=%v", exists, err)
	}
	if exists, err := repo.IngestHandoffExists(ctx, idTenantA, "conn_a", "col_b", "obj_a", 3); err != nil || exists {
		t.Fatalf("generation in other collection must not count: handoff=%v err=%v", exists, err)
	}
	if exists, err := repo.IngestHandoffExists(ctx, idTenantA, "conn_a", "col_a", "obj_a", 4); err != nil || exists {
		t.Fatalf("generation covers seq 3 only: handoff=%v err=%v", exists, err)
	}
}

// TestIngestFailed mirrors the document "failed" status for the newest
// generation: a failed embed job on a staging generation counts, a newer
// published generation clears it, and other collections never count.
func TestIngestFailed(t *testing.T) {
	db, repo, ctx := newRepo(t)
	seedTenant(t, ctx, db, idTenantA)
	if err := repo.CreateConnection(ctx, source.Connection{
		TenantID: idTenantA, ID: "conn_a", Kind: "slack", Name: "slack", CredentialRef: "ref"}); err != nil {
		t.Fatalf("create connection: %v", err)
	}
	if _, err := repo.UpsertObject(ctx, source.Object{
		TenantID: idTenantA, ConnectionID: "conn_a", ID: "obj_a", Kind: "message",
		NativeID: "slack:C1:1.0", AuthorizationMode: source.AuthorizationSourceACL, ObservedSeq: 1}); err != nil {
		t.Fatalf("upsert object: %v", err)
	}
	if err := repo.InsertObjectVersion(ctx, idTenantA, source.ObjectVersion{
		ObjectID: "obj_a", ObservedSeq: 2, ArtifactRef: "art_a", SHA256: "aa", ByteSize: 1, MimeType: "text/plain"}); err != nil {
		t.Fatalf("insert version: %v", err)
	}
	for _, col := range []string{"col_a", "col_b"} {
		if _, err := db.SQL.ExecContext(ctx, `
			INSERT INTO knowledge_collections (tenant_id, id, name, qdrant_collection)
			VALUES ($1, $2, $2, 'qdr_' || $2)`, idTenantA, col); err != nil {
			t.Fatalf("insert collection %s: %v", col, err)
		}
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.SQL.ExecContext(ctx, query, append([]any{idTenantA}, args...)...); err != nil {
			t.Fatal(err)
		}
	}
	assert := func(collection string, want bool, note string) {
		t.Helper()
		got, err := repo.IngestFailed(ctx, idTenantA, collection, "obj_a", 2)
		if err != nil || got != want {
			t.Fatalf("%s: failed=%v err=%v, want %v", note, got, err, want)
		}
	}
	assert("col_a", false, "no generation")

	exec(`INSERT INTO index_generations (tenant_id, id, collection_id, object_version_id, generation, state)
		VALUES ($1, 'gen_1', 'col_a', 'obj_a#2', 1, 'staging')`)
	exec(`INSERT INTO jobs (tenant_id, id, operation, status, connection_id, object_id, observed_revision,
		                  idempotency_key, input_ref, input_hash, principal, submitter_key, policy)
		VALUES ($1, 'job_embed_1', 'rag.embed', 'failed', 'conn_a', 'obj_a', 2,
		        'key-embed-1', 'obj_a#2', 'h', '{}', 'sub', '{"generation":"1"}'::jsonb)`)
	assert("col_a", true, "staging generation whose embed job failed")
	assert("col_b", false, "failure in another collection")

	exec(`INSERT INTO index_generations (tenant_id, id, collection_id, object_version_id, generation, state)
		VALUES ($1, 'gen_2', 'col_a', 'obj_a#2', 2, 'published')`)
	assert("col_a", false, "newer published generation")

	exec(`INSERT INTO index_generations (tenant_id, id, collection_id, object_version_id, generation, state)
		VALUES ($1, 'gen_3', 'col_a', 'obj_a#2', 3, 'failed')`)
	assert("col_a", true, "newest generation failed")
}
