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
		})
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
		})
	}); err != nil {
		t.Fatalf("apply snapshot 2: %v", err)
	}
	allow, deny = countGrants()
	if allow != 2 || deny != 0 {
		t.Fatalf("expected rev1+rev2 allows and deny pruned, got allow=%d deny=%d", allow, deny)
	}

	// Revision 3 never completes: begin + insert rows via a rolled-back
	// apply leaves everything intact.
	if err := repo.BeginSnapshot(ctx, idTenantA, "conn_a", source.SnapshotGrants, 3); err != nil {
		t.Fatalf("begin snapshot 3: %v", err)
	}
	before, _ := countGrants()
	err := repo.WithTx(ctx, func(tx source.Tx) error {
		if err := repo.ApplyAccessSnapshot(ctx, tx, idTenantA, "conn_a", 3, nil); err != nil {
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
		SELECT status FROM access_snapshots
		WHERE tenant_id = $1 AND connection_id = 'conn_a' AND kind = 'grants' AND revision = 3`,
		idTenantA).Scan(&status); err != nil {
		t.Fatalf("read snapshot 3: %v", err)
	}
	if status != "incomplete" {
		t.Fatalf("snapshot 3 status = %q, want incomplete", status)
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
		return repo.ApplyMembershipSnapshot(ctx, tx, idTenantA, "conn_a", 1, []source.SourceGroupMembership{member("sid_u1", 1)})
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
	groups, err := repo.GroupIDsForIdentity(ctx, idTenantA, "conn_a", "sid_u1")
	if err != nil {
		t.Fatalf("groups for sid_u1: %v", err)
	}
	if len(groups) != 1 || groups[0] != "grp_1" {
		t.Fatalf("expected sid_u1 still in grp_1 at latest complete rev, got %v", groups)
	}
	groups, err = repo.GroupIDsForIdentity(ctx, idTenantA, "conn_a", "sid_u2")
	if err != nil {
		t.Fatalf("groups for sid_u2: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("incomplete rev2 membership must not be authoritative, got %v", groups)
	}

	// Completing revision 2 replaces the set atomically.
	if err := repo.WithTx(ctx, func(tx source.Tx) error {
		return repo.ApplyMembershipSnapshot(ctx, tx, idTenantA, "conn_a", 2, []source.SourceGroupMembership{member("sid_u2", 2)})
	}); err != nil {
		t.Fatalf("apply rev2: %v", err)
	}
	groups, err = repo.GroupIDsForIdentity(ctx, idTenantA, "conn_a", "sid_u1")
	if err != nil {
		t.Fatalf("groups for sid_u1 post-rev2: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("sid_u1 membership should be retired, got %v", groups)
	}
	groups, err = repo.GroupIDsForIdentity(ctx, idTenantA, "conn_a", "sid_u2")
	if err != nil {
		t.Fatalf("groups for sid_u2 post-rev2: %v", err)
	}
	if len(groups) != 1 || groups[0] != "grp_1" {
		t.Fatalf("sid_u2 should be in grp_1, got %v", groups)
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
