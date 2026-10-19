package source_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

type fixture struct {
	t      *testing.T
	db     *postgres.DB
	repo   *postgres.SourceRepo
	ctx    context.Context
	tenant string
	users  map[string]string // friendly name -> user.ID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "tenant-a")
	return &fixture{t: t, db: db, repo: postgres.NewSourceRepo(db),
		ctx: context.Background(), tenant: tenant, users: map[string]string{}}
}

func (f *fixture) user(name string) string {
	f.t.Helper()
	if id, ok := f.users[name]; ok {
		return id
	}
	u := testdb.SeedUser(f.t, f.db, f.tenant, name+"@test.local", nil)
	f.users[name] = u.ID
	return u.ID
}

func (f *fixture) conn(id, status string) {
	f.t.Helper()
	if err := f.repo.CreateConnection(f.ctx, source.Connection{
		TenantID: f.tenant, ID: id, Kind: "github", Name: id,
		CredentialRef: "ref", Status: status}); err != nil {
		f.t.Fatalf("conn %s: %v", id, err)
	}
}

func (f *fixture) obj(id, connID, mode string) source.Object {
	f.t.Helper()
	o, err := f.repo.UpsertObject(f.ctx, source.Object{
		TenantID: f.tenant, ConnectionID: connID, ID: id, Kind: "repo",
		NativeID: "native-" + id, AuthorizationMode: mode, ObservedSeq: 1})
	if err != nil {
		f.t.Fatalf("obj %s: %v", id, err)
	}
	return o
}

func (f *fixture) identity(id, native, userName string) {
	f.t.Helper()
	if err := f.repo.InsertSourceIdentity(f.ctx, source.SourceIdentity{
		TenantID: f.tenant, ConnectionID: "conn_a", SourceKind: "github",
		ID: id, NativeID: native, MappedUserID: f.user(userName)}); err != nil {
		f.t.Fatalf("identity %s: %v", id, err)
	}
}

func (f *fixture) group(id, native string) {
	f.t.Helper()
	if err := f.repo.InsertSourceGroup(f.ctx, source.SourceGroup{
		TenantID: f.tenant, ConnectionID: "conn_a", SourceKind: "github",
		ID: id, NativeID: native, Name: id}); err != nil {
		f.t.Fatalf("group %s: %v", id, err)
	}
}

func (f *fixture) completeSnapshot(kind string, rev int64, completedAgo time.Duration) {
	f.t.Helper()
	if err := f.repo.BeginSnapshot(f.ctx, f.tenant, "conn_a", kind, rev); err != nil {
		f.t.Fatalf("begin %s rev %d: %v", kind, rev, err)
	}
	at := time.Now().Add(-completedAgo)
	if _, err := f.db.SQL.ExecContext(f.ctx, `
		UPDATE access_snapshots SET status = 'complete', completed_at = $4
		WHERE tenant_id = $1 AND connection_id = 'conn_a' AND kind = $2 AND revision = $3`,
		f.tenant, kind, rev, at); err != nil {
		f.t.Fatalf("complete %s rev %d: %v", kind, rev, err)
	}
}

func (f *fixture) grant(objID, kind, subjectID, effect string, rev int64) {
	f.t.Helper()
	var userID, groupID any
	if kind == source.GrantSubjectGroup {
		groupID = subjectID
	} else {
		userID = subjectID
	}
	if _, err := f.db.SQL.ExecContext(f.ctx, `
		INSERT INTO source_grants
		    (tenant_id, id, object_id, subject_type, source_identity_id, source_group_id, effect, access_revision)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		f.tenant, fmt.Sprintf("g:%s:%s:%s:%d", objID, subjectID, effect, rev),
		objID, kind, userID, groupID, effect, rev); err != nil {
		f.t.Fatalf("grant %s %s %s: %v", objID, subjectID, effect, err)
	}
}

func (f *fixture) member(groupID, identID string, rev int64) {
	f.t.Helper()
	if err := f.repo.InsertSourceMembership(f.ctx, source.SourceGroupMembership{
		TenantID: f.tenant, ConnectionID: "conn_a", GroupID: groupID,
		MemberID: identID, AccessRevision: rev}); err != nil {
		f.t.Fatalf("member %s->%s: %v", identID, groupID, err)
	}
}

func (f *fixture) principal(name string) auth.Principal {
	id := f.user(name)
	return auth.Principal{TenantID: f.tenant, ActorID: id, UserID: id}
}

func mustDeny(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, source.ErrDenied) {
		t.Fatalf("expected ErrDenied, got %v", err)
	}
}

func TestCanReadOwner(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	obj := f.obj("obj_o", "conn_a", source.AuthorizationOwner)
	owner := f.user("alice")
	if _, err := f.db.SQL.ExecContext(f.ctx,
		"UPDATE source_objects SET owner_actor_id = $3 WHERE tenant_id = $1 AND id = $2",
		f.tenant, "obj_o", owner); err != nil {
		t.Fatalf("set owner: %v", err)
	}
	obj, _ = f.repo.GetObject(f.ctx, f.tenant, "obj_o")

	a := source.NewAuthorizer(f.repo, time.Hour)
	if err := a.CanRead(f.ctx, f.principal("alice"), obj, source.ReadOptions{}); err != nil {
		t.Fatalf("owner must read: %v", err)
	}
	mustDeny(t, a.CanRead(f.ctx, f.principal("bob"), obj, source.ReadOptions{}))
}

func TestCanReadAudienceAndPublished(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	alice := f.user("alice")
	for _, mode := range []string{source.AuthorizationAudience, source.AuthorizationPublishedAudience} {
		obj := f.obj("obj_"+mode, "conn_a", mode)
		if _, err := f.db.SQL.ExecContext(f.ctx,
			"UPDATE source_objects SET audience = $3::jsonb WHERE tenant_id = $1 AND id = $2",
			f.tenant, obj.ID, fmt.Sprintf("[\"%s\"]", alice)); err != nil {
			t.Fatalf("set audience: %v", err)
		}
		obj, _ = f.repo.GetObject(f.ctx, f.tenant, obj.ID)
		a := source.NewAuthorizer(f.repo, time.Hour)
		if err := a.CanRead(f.ctx, f.principal("alice"), obj, source.ReadOptions{}); err != nil {
			t.Fatalf("%s member must read: %v", mode, err)
		}
		mustDeny(t, a.CanRead(f.ctx, f.principal("mallory"), obj, source.ReadOptions{}))
	}
}

// TestSourceACLDenyPrecedence: a principal with a group allow AND an explicit
// user deny at the current complete revision is denied.
func TestSourceACLDenyPrecedence(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	obj := f.obj("obj_acl", "conn_a", source.AuthorizationSourceACL)
	f.identity("sid_alice", "alice-gh", "alice")
	f.group("grp_devs", "devs")
	f.completeSnapshot(source.SnapshotMembership, 1, 0)
	f.completeSnapshot(source.SnapshotGrants, 1, 0)
	f.member("grp_devs", "sid_alice", 1)
	f.grant(obj.ID, source.GrantSubjectGroup, "grp_devs", source.GrantEffectAllow, 1)
	f.grant(obj.ID, source.GrantSubjectUser, "sid_alice", source.GrantEffectDeny, 1)

	a := source.NewAuthorizer(f.repo, time.Hour)
	mustDeny(t, a.CanRead(f.ctx, f.principal("alice"), obj, source.ReadOptions{}))
}

// TestSourceACLAllow: applicable allow at a fresh complete revision passes.
func TestSourceACLAllow(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	obj := f.obj("obj_acl", "conn_a", source.AuthorizationSourceACL)
	f.identity("sid_alice", "alice-gh", "alice")
	f.group("grp_devs", "devs")
	f.completeSnapshot(source.SnapshotMembership, 1, 0)
	f.completeSnapshot(source.SnapshotGrants, 1, 0)
	f.member("grp_devs", "sid_alice", 1)
	f.grant(obj.ID, source.GrantSubjectGroup, "grp_devs", source.GrantEffectAllow, 1)

	a := source.NewAuthorizer(f.repo, time.Hour)
	if err := a.CanRead(f.ctx, f.principal("alice"), obj, source.ReadOptions{}); err != nil {
		t.Fatalf("group allow must read: %v", err)
	}
}

func TestSourceACLDenials(t *testing.T) {
	newACLFixture := func(t *testing.T) (*fixture, source.Object) {
		f := newFixture(t)
		f.conn("conn_a", "active")
		obj := f.obj("obj_acl", "conn_a", source.AuthorizationSourceACL)
		return f, obj
	}
	a := func(f *fixture) *source.Authorizer { return source.NewAuthorizer(f.repo, time.Hour) }

	t.Run("no grant at complete snapshot", func(t *testing.T) {
		f, obj := newACLFixture(t)
		f.identity("sid_alice", "alice-gh", "alice")
		f.completeSnapshot(source.SnapshotMembership, 1, 0)
		f.completeSnapshot(source.SnapshotGrants, 1, 0)
		mustDeny(t, a(f).CanRead(f.ctx, f.principal("alice"), obj, source.ReadOptions{}))
	})

	t.Run("stale snapshot", func(t *testing.T) {
		f, obj := newACLFixture(t)
		f.identity("sid_alice", "alice-gh", "alice")
		f.completeSnapshot(source.SnapshotMembership, 1, 0)
		f.completeSnapshot(source.SnapshotGrants, 1, 3*time.Hour)
		f.grant(obj.ID, source.GrantSubjectUser, "sid_alice", source.GrantEffectAllow, 1)
		mustDeny(t, a(f).CanRead(f.ctx, f.principal("alice"), obj, source.ReadOptions{}))
	})

	t.Run("incomplete snapshot only", func(t *testing.T) {
		f, obj := newACLFixture(t)
		f.identity("sid_alice", "alice-gh", "alice")
		f.completeSnapshot(source.SnapshotMembership, 1, 0)
		// grants snapshot exists but never completed
		if err := f.repo.BeginSnapshot(f.ctx, f.tenant, "conn_a", source.SnapshotGrants, 1); err != nil {
			t.Fatalf("begin: %v", err)
		}
		mustDeny(t, a(f).CanRead(f.ctx, f.principal("alice"), obj, source.ReadOptions{}))
	})

	t.Run("missing identity mapping", func(t *testing.T) {
		f, obj := newACLFixture(t)
		f.completeSnapshot(source.SnapshotGrants, 1, 0)
		mustDeny(t, a(f).CanRead(f.ctx, f.principal("mallory"), obj, source.ReadOptions{}))
	})

	t.Run("tombstoned object with grant", func(t *testing.T) {
		f, obj := newACLFixture(t)
		f.identity("sid_alice", "alice-gh", "alice")
		f.completeSnapshot(source.SnapshotMembership, 1, 0)
		f.completeSnapshot(source.SnapshotGrants, 1, 0)
		f.grant(obj.ID, source.GrantSubjectUser, "sid_alice", source.GrantEffectAllow, 1)
		if err := f.repo.TombstoneObject(f.ctx, f.tenant, obj.ID); err != nil {
			t.Fatalf("tombstone: %v", err)
		}
		obj, _ = f.repo.GetObject(f.ctx, f.tenant, obj.ID)
		mustDeny(t, a(f).CanRead(f.ctx, f.principal("alice"), obj, source.ReadOptions{}))
	})

	t.Run("disabled connection", func(t *testing.T) {
		f, obj := newACLFixture(t)
		f.identity("sid_alice", "alice-gh", "alice")
		f.completeSnapshot(source.SnapshotMembership, 1, 0)
		f.completeSnapshot(source.SnapshotGrants, 1, 0)
		f.grant(obj.ID, source.GrantSubjectUser, "sid_alice", source.GrantEffectAllow, 1)
		if err := f.repo.UpdateConnectionStatus(f.ctx, f.tenant, "conn_a", "disabled"); err != nil {
			t.Fatalf("disable: %v", err)
		}
		mustDeny(t, a(f).CanRead(f.ctx, f.principal("alice"), obj, source.ReadOptions{}))
	})
}

// TestUnknownForbiddenIdentical proves unknown and forbidden objects produce
// the same public outcome.
func TestUnknownForbiddenIdentical(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	f.obj("obj_priv", "conn_a", source.AuthorizationAudience)

	_, unknownErr := f.repo.GetObject(f.ctx, f.tenant, "does-not-exist")
	obj, _ := f.repo.GetObject(f.ctx, f.tenant, "obj_priv")
	forbiddenErr := source.NewAuthorizer(f.repo, time.Hour).
		CanRead(f.ctx, f.principal("alice"), obj, source.ReadOptions{})
	if !errors.Is(unknownErr, source.ErrDenied) || !errors.Is(forbiddenErr, source.ErrDenied) {
		t.Fatalf("unknown=%v forbidden=%v must both be ErrDenied", unknownErr, forbiddenErr)
	}
	if unknownErr.Error() != forbiddenErr.Error() {
		t.Fatalf("error messages differ: %q vs %q", unknownErr, forbiddenErr)
	}
}

// TestRestrictionNarrowing proves request options can only tighten the
// persisted deny set — attempting to drop a denied capability is rejected.
func TestRestrictionNarrowing(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	obj := f.obj("obj_o", "conn_a", source.AuthorizationOwner)
	owner := f.user("alice")
	if _, err := f.db.SQL.ExecContext(f.ctx,
		"UPDATE source_objects SET owner_actor_id = $3 WHERE tenant_id = $1 AND id = $2",
		f.tenant, "obj_o", owner); err != nil {
		t.Fatalf("set owner: %v", err)
	}
	obj, _ = f.repo.GetObject(f.ctx, f.tenant, "obj_o")
	if err := f.repo.SetRestriction(f.ctx, source.SourceRestriction{
		TenantID: f.tenant, SubjectKind: source.RestrictionObject, SubjectID: obj.ID,
		RequiredAssurance: "high", DeniedCapabilities: []string{"public_endpoint"}}); err != nil {
		t.Fatalf("set restriction: %v", err)
	}

	a := source.NewAuthorizer(f.repo, time.Hour)
	eff, err := a.EffectiveRestrictions(f.ctx, obj, nil, "")
	if err != nil {
		t.Fatalf("effective restrictions: %v", err)
	}
	if eff.RequiredAssurance != "high" || len(eff.DeniedCapabilities) != 1 {
		t.Fatalf("effective = %+v", eff)
	}

	// Widening request (drops the persisted deny) is denied.
	widen := &source.SourceRestriction{DeniedCapabilities: []string{}, RequiredAssurance: "standard"}
	mustDeny(t, a.CanRead(f.ctx, f.principal("alice"), obj, source.ReadOptions{RequestedRestriction: widen}))

	// Narrowing request (keeps the deny, adds another, raises assurance) passes.
	narrow := &source.SourceRestriction{
		DeniedCapabilities: []string{"public_endpoint", "egress"},
		RequiredAssurance:  "confidential", // unrecognized label ranks highest
	}
	if err := a.CanRead(f.ctx, f.principal("alice"), obj, source.ReadOptions{RequestedRestriction: narrow}); err != nil {
		t.Fatalf("narrowing request must pass: %v", err)
	}
}
