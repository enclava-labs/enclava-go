package source_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
)

// conn-a-independent fixture helpers: the shared fixture binds
// identity/group/snapshot helpers to conn_a, so these take the connection
// explicitly for the multi-connection matrix.
func (f *fixture) objWith(o source.Object) source.Object {
	f.t.Helper()
	o.TenantID = f.tenant
	if o.Kind == "" {
		o.Kind = "repo"
	}
	if o.NativeID == "" {
		o.NativeID = "n-" + o.ID
	}
	if o.ObservedSeq == 0 {
		o.ObservedSeq = 1
	}
	out, err := f.repo.UpsertObject(f.ctx, o)
	if err != nil {
		f.t.Fatalf("obj %s: %v", o.ID, err)
	}
	return out
}

func (f *fixture) identityOn(connID, id, native, userName string) {
	f.t.Helper()
	if err := f.repo.InsertSourceIdentity(f.ctx, source.SourceIdentity{
		TenantID: f.tenant, ConnectionID: connID, SourceKind: "github",
		ID: id, NativeID: native, MappedUserID: f.user(userName)}); err != nil {
		f.t.Fatalf("identity %s: %v", id, err)
	}
}

func (f *fixture) groupOn(connID, id, native string) {
	f.t.Helper()
	if err := f.repo.InsertSourceGroup(f.ctx, source.SourceGroup{
		TenantID: f.tenant, ConnectionID: connID, SourceKind: "github",
		ID: id, NativeID: native, Name: id}); err != nil {
		f.t.Fatalf("group %s: %v", id, err)
	}
}

func (f *fixture) memberOn(connID, groupID, identID string, rev int64) {
	f.t.Helper()
	if err := f.repo.InsertSourceMembership(f.ctx, source.SourceGroupMembership{
		TenantID: f.tenant, ConnectionID: connID, GroupID: groupID,
		MemberID: identID, AccessRevision: rev}); err != nil {
		f.t.Fatalf("member %s->%s: %v", identID, groupID, err)
	}
}

func (f *fixture) completeSnapshotOn(connID, kind string, rev int64, completedAgo time.Duration) {
	f.t.Helper()
	if err := f.repo.BeginSnapshot(f.ctx, f.tenant, connID, kind, rev); err != nil {
		f.t.Fatalf("begin %s rev %d: %v", kind, rev, err)
	}
	at := time.Now().Add(-completedAgo)
	if _, err := f.db.SQL.ExecContext(f.ctx, `
		UPDATE access_snapshots SET status = 'complete', completed_at = $4
		WHERE tenant_id = $1 AND connection_id = $5 AND kind = $2 AND revision = $3`,
		f.tenant, kind, rev, at, connID); err != nil {
		f.t.Fatalf("complete %s rev %d: %v", kind, rev, err)
	}
}

// TestCanReadManyEquivalent builds one fixture covering every CanRead
// decision path and asserts the batch outcome equals the per-object outcome
// for each object — both the error's nil-ness and its exact value.
func TestCanReadManyEquivalent(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	f.conn("conn_b", "active")
	f.conn("conn_stale", "active")
	f.conn("conn_nosnap", "active")
	f.conn("conn_off", "inactive")

	f.identityOn("conn_a", "sid_a_alice", "alice", "alice")
	f.identityOn("conn_b", "sid_b_alice", "alice", "alice")
	f.identityOn("conn_stale", "sid_s_alice", "alice", "alice")
	f.groupOn("conn_a", "grp_a_devs", "devs")
	f.groupOn("conn_a", "grp_a_other", "other")
	f.groupOn("conn_b", "grp_b_devs", "devs")
	f.groupOn("conn_b", "grp_b_solo", "solo")

	f.completeSnapshotOn("conn_a", source.SnapshotGrants, 1, 0)
	f.completeSnapshotOn("conn_a", source.SnapshotMembership, 1, 0)
	f.completeSnapshotOn("conn_b", source.SnapshotGrants, 1, 0)
	// conn_b membership is observed but the snapshot never completes —
	// membership stays unproven.
	f.memberOn("conn_b", "grp_b_devs", "sid_b_alice", 1)
	f.completeSnapshotOn("conn_stale", source.SnapshotGrants, 1, 3*time.Hour)
	f.completeSnapshotOn("conn_off", source.SnapshotGrants, 1, 0)
	// conn_nosnap has no complete grants snapshot at all.

	alice := f.user("alice")
	f.memberOn("conn_a", "grp_a_devs", "sid_a_alice", 1)

	objOwner := f.objWith(source.Object{ID: "obj_owner", AuthorizationMode: source.AuthorizationOwner, OwnerActorID: alice})
	objOwnerBob := f.objWith(source.Object{ID: "obj_owner_bob", AuthorizationMode: source.AuthorizationOwner, OwnerActorID: f.user("bob")})
	objPub := f.objWith(source.Object{ID: "obj_pub", AuthorizationMode: source.AuthorizationPublishedAudience, Audience: []string{f.user("mallory")}})
	objAud := f.objWith(source.Object{ID: "obj_aud", AuthorizationMode: source.AuthorizationAudience, Audience: []string{alice}})
	objAudStar := f.objWith(source.Object{ID: "obj_aud_star", AuthorizationMode: source.AuthorizationAudience, Audience: []string{"*"}})
	// The schema rejects unknown modes, so this decision path is in-memory only.
	objNoMode := source.Object{TenantID: f.tenant, ID: "obj_nomode", Kind: "repo", NativeID: "n-obj_nomode", ObservedSeq: 1, AuthorizationMode: "weird"}
	objACLAllow := f.objWith(source.Object{ID: "obj_acl_allow", ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationSourceACL})
	objACLDenyWins := f.objWith(source.Object{ID: "obj_acl_denywins", ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationSourceACL})
	objACLAll := f.objWith(source.Object{ID: "obj_acl_all", ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationSourceACL})
	objACLAllDeny := f.objWith(source.Object{ID: "obj_acl_alldeny", ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationSourceACL})
	objACLGroupAllow := f.objWith(source.Object{ID: "obj_acl_grp_allow", ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationSourceACL})
	objACLGroupMiss := f.objWith(source.Object{ID: "obj_acl_grp_miss", ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationSourceACL})
	objACLGroupDeny := f.objWith(source.Object{ID: "obj_acl_grp_deny", ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationSourceACL})
	objACLNoGrant := f.objWith(source.Object{ID: "obj_acl_nogrant", ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationSourceACL})
	objACLParent := f.objWith(source.Object{ID: "obj_acl_parent", ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationSourceACL})
	objACLChild := f.objWith(source.Object{ID: "obj_acl_child", ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationSourceACL})
	if err := f.repo.ObserveObject(f.ctx, f.tenant, objACLChild.ID, objACLParent.ID, nil); err != nil {
		t.Fatalf("link child: %v", err)
	}
	objBAllow := f.objWith(source.Object{ID: "obj_b_allow", ConnectionID: "conn_b", AuthorizationMode: source.AuthorizationSourceACL})
	objBGroupDeny := f.objWith(source.Object{ID: "obj_b_grp_deny", ConnectionID: "conn_b", AuthorizationMode: source.AuthorizationSourceACL})
	objBGroupOnly := f.objWith(source.Object{ID: "obj_b_grp_only", ConnectionID: "conn_b", AuthorizationMode: source.AuthorizationSourceACL})
	objStale := f.objWith(source.Object{ID: "obj_stale", ConnectionID: "conn_stale", AuthorizationMode: source.AuthorizationSourceACL})
	objNosnap := f.objWith(source.Object{ID: "obj_nosnap", ConnectionID: "conn_nosnap", AuthorizationMode: source.AuthorizationSourceACL})
	objOff := f.objWith(source.Object{ID: "obj_off", ConnectionID: "conn_off", AuthorizationMode: source.AuthorizationSourceACL})
	objTomb := f.objWith(source.Object{ID: "obj_tomb", ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationSourceACL})
	if err := f.repo.TombstoneObject(f.ctx, f.tenant, objTomb.ID); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	objTomb, _ = f.repo.GetObject(f.ctx, f.tenant, objTomb.ID)
	objConnless := f.objWith(source.Object{ID: "obj_connless", AuthorizationMode: source.AuthorizationAudience, Audience: []string{alice}})
	objConnlessACL := f.objWith(source.Object{ID: "obj_connless_acl", AuthorizationMode: source.AuthorizationSourceACL})
	phantom := source.Object{TenantID: f.tenant, ID: "obj_phantom", ConnectionID: "conn_a", Kind: "repo", NativeID: "n-phantom", AuthorizationMode: source.AuthorizationSourceACL, ObservedSeq: 1}

	// Grants at conn_a's complete revision 1.
	f.grant(objACLAllow.ID, source.GrantSubjectUser, "sid_a_alice", source.GrantEffectAllow, 1)
	f.grant(objACLDenyWins.ID, source.GrantSubjectUser, "sid_a_alice", source.GrantEffectAllow, 1)
	f.grant(objACLDenyWins.ID, source.GrantSubjectUser, "sid_a_alice", source.GrantEffectDeny, 1)
	f.grant(objACLAll.ID, source.GrantSubjectAll, "", source.GrantEffectAllow, 1)
	f.grant(objACLAllDeny.ID, source.GrantSubjectAll, "", source.GrantEffectDeny, 1)
	f.grant(objACLAllDeny.ID, source.GrantSubjectUser, "sid_a_alice", source.GrantEffectAllow, 1)
	f.grant(objACLGroupAllow.ID, source.GrantSubjectGroup, "grp_a_devs", source.GrantEffectAllow, 1)
	f.grant(objACLGroupMiss.ID, source.GrantSubjectGroup, "grp_a_other", source.GrantEffectAllow, 1)
	f.grant(objACLGroupDeny.ID, source.GrantSubjectUser, "sid_a_alice", source.GrantEffectAllow, 1)
	f.grant(objACLGroupDeny.ID, source.GrantSubjectGroup, "grp_a_devs", source.GrantEffectDeny, 1)
	f.grant(objACLParent.ID, source.GrantSubjectUser, "sid_a_alice", source.GrantEffectAllow, 1)
	f.grant(objTomb.ID, source.GrantSubjectUser, "sid_a_alice", source.GrantEffectAllow, 1)
	// Grants at conn_b's complete revision 1.
	f.grant(objBAllow.ID, source.GrantSubjectUser, "sid_b_alice", source.GrantEffectAllow, 1)
	f.grant(objBGroupDeny.ID, source.GrantSubjectUser, "sid_b_alice", source.GrantEffectAllow, 1)
	f.grant(objBGroupDeny.ID, source.GrantSubjectGroup, "grp_b_devs", source.GrantEffectDeny, 1)
	f.grant(objBGroupOnly.ID, source.GrantSubjectGroup, "grp_b_solo", source.GrantEffectAllow, 1)
	// Stale and no-snapshot connections.
	f.grant(objStale.ID, source.GrantSubjectUser, "sid_s_alice", source.GrantEffectAllow, 1)
	f.grant(objNosnap.ID, source.GrantSubjectAll, "", source.GrantEffectAllow, 1)
	f.grant(objOff.ID, source.GrantSubjectAll, "", source.GrantEffectAllow, 1)

	objects := []source.Object{
		objOwner, objOwnerBob, objAud, objAudStar, objPub, objNoMode,
		objACLAllow, objACLDenyWins, objACLAll, objACLAllDeny,
		objACLGroupAllow, objACLGroupMiss, objACLGroupDeny, objACLNoGrant,
		objACLChild, objACLParent,
		objBAllow, objBGroupDeny, objBGroupOnly,
		objStale, objNosnap, objOff, objTomb, objConnless, objConnlessACL,
		phantom,
	}
	// Expected outcomes for alice pin the matrix semantics, not just parity.
	wantAlice := map[string]error{
		"obj_owner": nil, "obj_owner_bob": source.ErrDenied,
		"obj_aud": nil, "obj_aud_star": nil, "obj_pub": source.ErrDenied,
		"obj_nomode":    source.ErrDenied,
		"obj_acl_allow": nil, "obj_acl_denywins": source.ErrDenied,
		"obj_acl_all": nil, "obj_acl_alldeny": source.ErrDenied,
		"obj_acl_grp_allow": nil, "obj_acl_grp_miss": source.ErrDenied,
		"obj_acl_grp_deny": source.ErrDenied, "obj_acl_nogrant": source.ErrDenied,
		"obj_acl_child": nil, "obj_acl_parent": nil,
		"obj_b_allow": nil, "obj_b_grp_deny": source.ErrDenied,
		"obj_b_grp_only": source.ErrDenied,
		"obj_stale":      source.ErrDenied, "obj_nosnap": source.ErrDenied,
		"obj_off": source.ErrDenied, "obj_tomb": source.ErrDenied,
		"obj_connless": nil, "obj_connless_acl": source.ErrDenied,
		"obj_phantom": source.ErrDenied,
	}

	a := source.NewAuthorizer(f.repo, time.Hour)
	for _, name := range []string{"alice", "bob", "mallory"} {
		p := f.principal(name)
		got := a.CanReadMany(f.ctx, p, objects, source.ReadOptions{})
		if len(got) != len(objects) {
			t.Fatalf("%s: CanReadMany returned %d results for %d objects", name, len(got), len(objects))
		}
		for i, obj := range objects {
			want := a.CanRead(f.ctx, p, obj, source.ReadOptions{})
			if want == nil && got[i] != nil {
				t.Fatalf("%s %s: batch=%v, single=nil", name, obj.ID, got[i])
			}
			if want != nil && !errors.Is(got[i], want) {
				t.Fatalf("%s %s: batch=%v, single=%v", name, obj.ID, got[i], want)
			}
			if name == "alice" {
				if exp, pinned := wantAlice[obj.ID]; pinned && !errors.Is(got[i], exp) {
					t.Fatalf("alice %s: got %v, expected %v", obj.ID, got[i], exp)
				}
			}
		}
	}

	// Request narrowing must behave identically per object: a widening
	// request denies, a narrower request allows — same for single and batch.
	if err := f.repo.SetRestriction(f.ctx, source.SourceRestriction{
		TenantID: f.tenant, SubjectKind: source.RestrictionObject, SubjectID: objOwner.ID,
		RequiredAssurance: "high", DeniedCapabilities: []string{"public_endpoint"}}); err != nil {
		t.Fatalf("set restriction: %v", err)
	}
	narrowing := []source.ReadOptions{
		{RequestedRestriction: &source.SourceRestriction{DeniedCapabilities: []string{}, RequiredAssurance: "standard"}},
		{RequestedRestriction: &source.SourceRestriction{DeniedCapabilities: []string{"public_endpoint", "egress"}, RequiredAssurance: "confidential"}},
		{},
	}
	p := f.principal("alice")
	for _, opts := range narrowing {
		got := a.CanReadMany(f.ctx, p, objects, opts)
		for i, obj := range objects {
			want := a.CanRead(f.ctx, p, obj, opts)
			if want == nil && got[i] != nil {
				t.Fatalf("opts=%+v %s: batch=%v, single=nil", opts.RequestedRestriction, obj.ID, got[i])
			}
			if want != nil && !errors.Is(got[i], want) {
				t.Fatalf("opts=%+v %s: batch=%v, single=%v", opts.RequestedRestriction, obj.ID, got[i], want)
			}
		}
	}
}

// countingRepo wraps the postgres SourceRepo to make the batch's per
// connection query bound observable.
type countingRepo struct {
	*postgres.SourceRepo
	calls map[string]int
}

func (r *countingRepo) count(name string) {
	r.calls[name]++
}

func (r *countingRepo) GetConnection(ctx context.Context, tenantID, id string) (source.Connection, error) {
	r.count("GetConnection")
	return r.SourceRepo.GetConnection(ctx, tenantID, id)
}

func (r *countingRepo) LatestCompleteRevision(ctx context.Context, tenantID, connectionID, kind string) (int64, time.Time, error) {
	r.count("LatestCompleteRevision")
	return r.SourceRepo.LatestCompleteRevision(ctx, tenantID, connectionID, kind)
}

func (r *countingRepo) AncestorObjectIDs(ctx context.Context, tenantID, connectionID, objectID string, maxDepth int) ([]string, error) {
	r.count("AncestorObjectIDs")
	return r.SourceRepo.AncestorObjectIDs(ctx, tenantID, connectionID, objectID, maxDepth)
}

func (r *countingRepo) AncestorObjectIDsBatch(ctx context.Context, tenantID, connectionID string, objectIDs []string, maxDepth int) (map[string][]string, error) {
	r.count("AncestorObjectIDsBatch")
	return r.SourceRepo.AncestorObjectIDsBatch(ctx, tenantID, connectionID, objectIDs, maxDepth)
}

func (r *countingRepo) GrantsForObjects(ctx context.Context, tenantID, connectionID string, objectIDs []string, accessRevision int64) ([]source.SourceGrant, error) {
	r.count("GrantsForObjects")
	return r.SourceRepo.GrantsForObjects(ctx, tenantID, connectionID, objectIDs, accessRevision)
}

func (r *countingRepo) SourceIdentityForUser(ctx context.Context, tenantID, connectionID, userID string) (source.SourceIdentity, error) {
	r.count("SourceIdentityForUser")
	return r.SourceRepo.SourceIdentityForUser(ctx, tenantID, connectionID, userID)
}

func (r *countingRepo) GroupIDsForIdentity(ctx context.Context, tenantID, connectionID, sourceIdentityID string, revision int64) ([]string, error) {
	r.count("GroupIDsForIdentity")
	return r.SourceRepo.GroupIDsForIdentity(ctx, tenantID, connectionID, sourceIdentityID, revision)
}

func (r *countingRepo) RestrictionsForSubject(ctx context.Context, tenantID, subjectKind, subjectID string) ([]source.SourceRestriction, error) {
	r.count("RestrictionsForSubject")
	return r.SourceRepo.RestrictionsForSubject(ctx, tenantID, subjectKind, subjectID)
}

// TestCanReadManyBoundedRepoCalls: one batch over many objects of one
// connection must cost a constant number of repo calls, not one read chain
// per object.
func TestCanReadManyBoundedRepoCalls(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	f.identityOn("conn_a", "sid_a_alice", "alice", "alice")
	f.group("grp_devs", "devs")
	f.completeSnapshot(source.SnapshotGrants, 1, 0)
	f.completeSnapshot(source.SnapshotMembership, 1, 0)
	f.member("grp_devs", "sid_a_alice", 1)

	objects := make([]source.Object, 0, 30)
	for i := range 30 {
		obj := f.objWith(source.Object{ID: fmt.Sprintf("obj_bulk_%02d", i), ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationSourceACL})
		f.grant(obj.ID, source.GrantSubjectGroup, "grp_devs", source.GrantEffectAllow, 1)
		objects = append(objects, obj)
	}

	repo := &countingRepo{SourceRepo: f.repo, calls: map[string]int{}}
	a := source.NewAuthorizer(repo, time.Hour)
	errs := a.CanReadMany(f.ctx, f.principal("alice"), objects, source.ReadOptions{})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("object %d denied: %v", i, err)
		}
	}
	total := 0
	for name, n := range repo.calls {
		total += n
		t.Logf("%s: %d", name, n)
	}
	// Connection + grants revision + batched ancestors + grants + identity +
	// membership revision + memberships: at most 8 queries for any batch.
	if total > 8 {
		t.Fatalf("CanReadMany issued %d repo calls for %d objects on one connection; want a per-connection bound", total, len(objects))
	}
	if repo.calls["AncestorObjectIDsBatch"] != 1 || repo.calls["AncestorObjectIDs"] != 0 {
		t.Fatalf("ancestor lookup must be a single batch call: %+v", repo.calls)
	}
}
