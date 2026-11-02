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
	} else if kind == source.GrantSubjectUser {
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

func TestSourceACLAncestorGrants(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	root := f.obj("root", "conn_a", source.AuthorizationSourceACL)
	parent := f.obj("parent", "conn_a", source.AuthorizationSourceACL)
	child := f.obj("child", "conn_a", source.AuthorizationSourceACL)
	for _, edge := range [][2]string{{parent.ID, root.ID}, {child.ID, parent.ID}} {
		if _, err := f.db.SQL.ExecContext(f.ctx,
			`UPDATE source_objects SET parent_object_id = $3 WHERE tenant_id = $1 AND id = $2`,
			f.tenant, edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	f.identity("sid_alice", "alice-gh", "alice")
	f.completeSnapshot(source.SnapshotMembership, 1, 0)
	f.completeSnapshot(source.SnapshotGrants, 1, 0)
	f.grant(root.ID, source.GrantSubjectUser, "sid_alice", source.GrantEffectAllow, 1)
	a := source.NewAuthorizer(f.repo, time.Hour)
	if err := a.CanRead(f.ctx, f.principal("alice"), child, source.ReadOptions{}); err != nil {
		t.Fatalf("ancestor allow must apply: %v", err)
	}
	f.grant(parent.ID, source.GrantSubjectUser, "sid_alice", source.GrantEffectDeny, 1)
	mustDeny(t, a.CanRead(f.ctx, f.principal("alice"), child, source.ReadOptions{}))
	f.grant(child.ID, source.GrantSubjectUser, "sid_alice", source.GrantEffectAllow, 1)
	mustDeny(t, a.CanRead(f.ctx, f.principal("alice"), child, source.ReadOptions{}))
	if err := f.repo.TombstoneObject(f.ctx, f.tenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	mustDeny(t, a.CanRead(f.ctx, f.principal("alice"), child, source.ReadOptions{}))
}

func TestSourceACLAncestorChainBounds(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	f.identity("sid_alice", "alice-gh", "alice")
	f.completeSnapshot(source.SnapshotMembership, 1, 0)
	f.completeSnapshot(source.SnapshotGrants, 1, 0)
	objects := make([]source.Object, 10)
	var previous source.Object
	for i := range objects {
		objects[i] = f.obj(fmt.Sprintf("node_%d", i), "conn_a", source.AuthorizationSourceACL)
		if i > 0 {
			if _, err := f.db.SQL.ExecContext(f.ctx,
				`UPDATE source_objects SET parent_object_id = $3 WHERE tenant_id = $1 AND id = $2`,
				f.tenant, objects[i].ID, previous.ID); err != nil {
				t.Fatal(err)
			}
		}
		previous = objects[i]
	}
	f.grant(objects[0].ID, source.GrantSubjectUser, "sid_alice", source.GrantEffectAllow, 1)
	a := source.NewAuthorizer(f.repo, time.Hour)
	if err := a.CanRead(f.ctx, f.principal("alice"), objects[8], source.ReadOptions{}); err != nil {
		t.Fatalf("eight ancestor edges must be accepted: %v", err)
	}
	mustDeny(t, a.CanRead(f.ctx, f.principal("alice"), objects[9], source.ReadOptions{}))
	if _, err := f.db.SQL.ExecContext(f.ctx,
		`UPDATE source_objects SET parent_object_id = $3 WHERE tenant_id = $1 AND id = $2`,
		f.tenant, objects[0].ID, objects[1].ID); err != nil {
		t.Fatal(err)
	}
	mustDeny(t, a.CanRead(f.ctx, f.principal("alice"), objects[1], source.ReadOptions{}))
}

func TestSourceACLMembershipNeededForGroups(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	obj := f.obj("obj_acl", "conn_a", source.AuthorizationSourceACL)
	f.identity("sid_alice", "alice-gh", "alice")
	f.group("grp_devs", "devs")
	f.completeSnapshot(source.SnapshotGrants, 1, 0)
	f.grant(obj.ID, source.GrantSubjectUser, "sid_alice", source.GrantEffectAllow, 1)
	a := source.NewAuthorizer(f.repo, time.Hour)
	p := f.principal("alice")
	if err := a.CanRead(f.ctx, p, obj, source.ReadOptions{}); err != nil {
		t.Fatalf("direct grant without membership snapshot: %v", err)
	}
	f.grant(obj.ID, source.GrantSubjectGroup, "grp_devs", source.GrantEffectAllow, 1)
	if err := a.CanRead(f.ctx, p, obj, source.ReadOptions{}); err != nil {
		t.Fatalf("unknown group allow must not invalidate direct grant: %v", err)
	}
	f.grant(obj.ID, source.GrantSubjectGroup, "grp_devs", source.GrantEffectDeny, 1)
	mustDeny(t, a.CanRead(f.ctx, p, obj, source.ReadOptions{}))
	f.completeSnapshot(source.SnapshotMembership, 1, 3*time.Hour)
	mustDeny(t, a.CanRead(f.ctx, p, obj, source.ReadOptions{}))
	f.completeSnapshot(source.SnapshotMembership, 2, 0)
	if err := a.CanRead(f.ctx, p, obj, source.ReadOptions{}); err != nil {
		t.Fatalf("fresh non-member must retain direct grant: %v", err)
	}
	f.member("grp_devs", "sid_alice", 2)
	mustDeny(t, a.CanRead(f.ctx, p, obj, source.ReadOptions{}))
}

func TestSourceACLEveryoneGrant(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	obj := f.obj("obj_acl", "conn_a", source.AuthorizationSourceACL)
	f.completeSnapshot(source.SnapshotGrants, 1, 0)
	f.grant(obj.ID, source.GrantSubjectAll, "", source.GrantEffectAllow, 1)
	a := source.NewAuthorizer(f.repo, time.Hour)
	p := f.principal("alice")
	if err := a.CanRead(f.ctx, p, obj, source.ReadOptions{}); err != nil {
		t.Fatalf("everyone allow does not require a source identity: %v", err)
	}
	f.grant(obj.ID, source.GrantSubjectAll, "", source.GrantEffectDeny, 1)
	mustDeny(t, a.CanRead(f.ctx, p, obj, source.ReadOptions{}))
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
		tokens, rev, err := a(f).ReadTokens(f.ctx, obj)
		if err != nil {
			t.Fatal(err)
		}
		tokensEqual(t, tokens, "sid:sid_alice")
		if rev != 1 {
			t.Fatalf("grants revision = %d, want 1", rev)
		}
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

func tokensEqual(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("tokens = %#v, want %#v", got, want)
	}
	have := map[string]bool{}
	for _, token := range got {
		have[token] = true
	}
	for _, token := range want {
		if !have[token] {
			t.Fatalf("tokens = %#v, missing %q", got, token)
		}
	}
}

func TestReadTokensOwnerAudienceDenyNone(t *testing.T) {
	f := newFixture(t)
	a := source.NewAuthorizer(f.repo, time.Hour)
	owner := source.Object{TenantID: f.tenant, ID: "o", AuthorizationMode: source.AuthorizationOwner, OwnerActorID: "actor-x"}
	tokens, rev, err := a.ReadTokens(f.ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	tokensEqual(t, tokens, "actor:actor-x")
	if rev != 0 {
		t.Fatalf("owner grants revision = %d, want 0", rev)
	}

	aud := source.Object{TenantID: f.tenant, ID: "o", AuthorizationMode: source.AuthorizationAudience, Audience: []string{"u1", "*"}}
	tokens, rev, err = a.ReadTokens(f.ctx, aud)
	if err != nil {
		t.Fatal(err)
	}
	tokensEqual(t, tokens, "actor:u1", "tenant")
	if rev != 0 {
		t.Fatalf("audience grants revision = %d, want 0", rev)
	}

	// Deny-everyone and dead objects project the sentinel, never [].
	dead := source.Object{TenantID: f.tenant, ID: "o", AuthorizationMode: source.AuthorizationAudience}
	tokens, rev, err = a.ReadTokens(f.ctx, dead)
	if err != nil {
		t.Fatal(err)
	}
	tokensEqual(t, tokens, source.TokenNone)
	if rev != 0 {
		t.Fatalf("empty audience grants revision = %d, want 0", rev)
	}
	now := time.Now()
	dead.TombstonedAt = &now
	dead.Audience = []string{"*"}
	tokens, rev, err = a.ReadTokens(f.ctx, dead)
	if err != nil {
		t.Fatal(err)
	}
	tokensEqual(t, tokens, source.TokenNone)
	if rev != 0 {
		t.Fatalf("tombstoned grants revision = %d, want 0", rev)
	}
}

func TestReadTokensSourceACLMirrorsAllowPaths(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	root := f.obj("root", "conn_a", source.AuthorizationSourceACL)
	child := f.obj("child", "conn_a", source.AuthorizationSourceACL)
	if _, err := f.db.SQL.ExecContext(f.ctx,
		`UPDATE source_objects SET parent_object_id = $3 WHERE tenant_id = $1 AND id = $2`,
		f.tenant, child.ID, root.ID); err != nil {
		t.Fatal(err)
	}
	f.identity("sid_alice", "alice-gh", "alice")
	f.group("grp_devs", "devs")
	f.completeSnapshot(source.SnapshotGrants, 1, 0)
	f.grant(child.ID, source.GrantSubjectUser, "sid_alice", source.GrantEffectAllow, 1)
	f.grant(root.ID, source.GrantSubjectGroup, "grp_devs", source.GrantEffectAllow, 1)
	f.grant(child.ID, source.GrantSubjectAll, "", source.GrantEffectDeny, 1) // deny-everyone still projects allows

	a := source.NewAuthorizer(f.repo, time.Hour)
	tokens, rev, err := a.ReadTokens(f.ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	// Allows project for the post-check to enforce the deny; ancestor grants inherit.
	tokensEqual(t, tokens, "sid:sid_alice", "sgrp:grp_devs")
	if rev != 1 {
		t.Fatalf("grants revision = %d, want 1", rev)
	}

	noAllow := f.obj("no-allow", "conn_a", source.AuthorizationSourceACL)
	f.grant(noAllow.ID, source.GrantSubjectAll, "", source.GrantEffectDeny, 1)
	tokens, rev, err = a.ReadTokens(f.ctx, noAllow)
	if err != nil {
		t.Fatal(err)
	}
	tokensEqual(t, tokens, source.TokenNone)
	if rev != 1 {
		t.Fatalf("deny-only grants revision = %d, want 1", rev)
	}

	// No grants snapshot → unreadable sentinel, mirroring CanRead's closed default.
	f.conn("conn_b", "active")
	orphan := f.obj("orphan", "conn_b", source.AuthorizationSourceACL)
	tokens, rev, err = a.ReadTokens(f.ctx, orphan)
	if err != nil {
		t.Fatal(err)
	}
	tokensEqual(t, tokens, source.TokenNone)
	if rev != 0 {
		t.Fatalf("missing snapshot grants revision = %d, want 0", rev)
	}
}

func TestQueryTokensIdentityAndGroups(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	f.conn("conn_b", "active")
	f.identity("sid_alice", "alice-gh", "alice")
	f.group("grp_devs", "devs")
	f.completeSnapshot(source.SnapshotMembership, 1, 0)
	f.member("grp_devs", "sid_alice", 1)

	a := source.NewAuthorizer(f.repo, time.Hour)
	p := f.principal("alice")
	tokens, err := a.QueryTokens(f.ctx, p, []string{"conn_a", "conn_b"})
	if err != nil {
		t.Fatal(err)
	}
	tokensEqual(t, tokens, "tenant", "actor:"+p.ActorID, "sid:sid_alice", "sgrp:grp_devs")

	// No complete membership snapshot → identity token only, groups unknown.
	f.conn("conn_c", "active")
	if err := f.repo.InsertSourceIdentity(f.ctx, source.SourceIdentity{
		TenantID: f.tenant, ConnectionID: "conn_c", SourceKind: "github",
		ID: "sid_c", NativeID: "alice-gh", MappedUserID: p.UserID}); err != nil {
		t.Fatal(err)
	}
	tokens, err = a.QueryTokens(f.ctx, p, []string{"conn_c"})
	if err != nil {
		t.Fatal(err)
	}
	tokensEqual(t, tokens, "tenant", "actor:"+p.ActorID, "sid:sid_c")

	// Delegation adds the delegating actor's token.
	p.DelegatedByActorID = "delegator"
	tokens, err = a.QueryTokens(f.ctx, p, []string{"conn_c"})
	if err != nil {
		t.Fatal(err)
	}
	tokensEqual(t, tokens, "tenant", "actor:"+p.ActorID, "actor:delegator", "sid:sid_c")
}
