package source_test

import (
	"context"
	"encoding/json"
	"errors"
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

// collection creates a knowledge_collections row; audience entries are actor
// IDs, "*" is the tenant-wide wildcard.
func (f *fixture) collection(id, owner string, active bool, audience ...string) {
	f.t.Helper()
	raw, err := json.Marshal(audience)
	if err != nil {
		f.t.Fatalf("audience json: %v", err)
	}
	if _, err := f.db.SQL.ExecContext(f.ctx, `
		INSERT INTO knowledge_collections
		    (tenant_id, id, name, embedding_model, qdrant_collection, owner_actor_id, is_active, audience)
		VALUES ($1, $2, $2, 'model', 'q_'||$2, NULLIF($3,''), $4, $5::jsonb)`,
		f.tenant, id, owner, active, raw); err != nil {
		f.t.Fatalf("collection %s: %v", id, err)
	}
}

// item places obj in collection; removed=true rows a removed collection_items row.
func (f *fixture) item(collectionID, objectID string, removed bool) {
	f.t.Helper()
	_, err := f.db.SQL.ExecContext(f.ctx, `
		INSERT INTO collection_items (tenant_id, id, collection_id, object_id, removed_at)
		VALUES ($1, $2, $3, $4, CASE WHEN $5 THEN now() ELSE NULL END)`,
		f.tenant, "it_"+collectionID+"_"+objectID, collectionID, objectID, removed)
	if err != nil {
		f.t.Fatalf("item %s/%s: %v", collectionID, objectID, err)
	}
}

func (f *fixture) principal(name string) auth.Principal {
	id := f.user(name)
	return auth.Principal{TenantID: f.tenant, ActorID: id, UserID: id}
}

func (f *fixture) admin() auth.Principal {
	return auth.Principal{TenantID: f.tenant, ActorID: "admin", Scopes: map[string]struct{}{"platform.admin": {}}}
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
		f.tenant, obj.ID, owner); err != nil {
		t.Fatalf("set owner: %v", err)
	}
	obj, _ = f.repo.GetObject(f.ctx, f.tenant, obj.ID)
	a := source.NewAuthorizer(f.repo)
	if err := a.CanRead(f.ctx, f.principal("alice"), obj, source.ReadOptions{}); err != nil {
		t.Fatalf("owner must read: %v", err)
	}
	p := auth.Principal{TenantID: f.tenant, ActorID: "agent-x", DelegatedByActorID: owner}
	if err := a.CanRead(f.ctx, p, obj, source.ReadOptions{}); err != nil {
		t.Fatalf("delegated owner must read: %v", err)
	}
	mustDeny(t, a.CanRead(f.ctx, f.principal("bob"), obj, source.ReadOptions{}))
}

func TestCanReadCollection(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	obj := f.obj("obj_c", "conn_a", source.AuthorizationCollection)
	alice, bob, carol := f.user("alice"), f.user("bob"), f.user("carol")

	// Two collections contain the object; only "readers" is readable by alice.
	f.collection("secret", carol, true)
	f.collection("readers", carol, true, alice)
	f.item("secret", obj.ID, false)
	f.item("readers", obj.ID, false)

	a := source.NewAuthorizer(f.repo)
	if err := a.CanRead(f.ctx, f.principal("alice"), obj, source.ReadOptions{}); err != nil {
		t.Fatalf("audience member must read: %v", err)
	}
	mustDeny(t, a.CanRead(f.ctx, f.principal("bob"), obj, source.ReadOptions{}))

	// Collection owner reads; the object owner_actor_id is irrelevant.
	f.collection("owned", bob, true)
	obj2 := f.obj("obj_d", "conn_a", source.AuthorizationCollection)
	f.item("owned", obj2.ID, false)
	if err := a.CanRead(f.ctx, f.principal("bob"), obj2, source.ReadOptions{}); err != nil {
		t.Fatalf("collection owner must read: %v", err)
	}

	// Wildcard audience reads for any tenant principal.
	f.collection("public", carol, true, "*")
	obj3 := f.obj("obj_e", "conn_a", source.AuthorizationCollection)
	f.item("public", obj3.ID, false)
	if err := a.CanRead(f.ctx, f.principal("bob"), obj3, source.ReadOptions{}); err != nil {
		t.Fatalf("wildcard audience must read: %v", err)
	}
}

func TestCanReadCollectionInactiveAndRemoved(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	obj := f.obj("obj_i", "conn_a", source.AuthorizationCollection)
	alice := f.user("alice")

	// Inactive collection denies non-admins even when the audience matches.
	f.collection("off", "", false, alice)
	f.item("off", obj.ID, false)
	a := source.NewAuthorizer(f.repo)
	mustDeny(t, a.CanRead(f.ctx, f.principal("alice"), obj, source.ReadOptions{}))
	if err := a.CanRead(f.ctx, f.admin(), obj, source.ReadOptions{}); err != nil {
		t.Fatalf("admin must read inactive collection objects: %v", err)
	}

	// Removed collection items do not confer read.
	f.collection("gone", "", true, alice)
	obj2 := f.obj("obj_j", "conn_a", source.AuthorizationCollection)
	f.item("gone", obj2.ID, true)
	mustDeny(t, a.CanRead(f.ctx, f.principal("alice"), obj2, source.ReadOptions{}))
}

func TestCanReadCollectionNoCollection(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	obj := f.obj("obj_n", "conn_a", source.AuthorizationCollection)
	a := source.NewAuthorizer(f.repo)
	mustDeny(t, a.CanRead(f.ctx, f.principal("alice"), obj, source.ReadOptions{}))
	mustDeny(t, a.CanRead(f.ctx, f.admin(), obj, source.ReadOptions{}))
}

func TestCanReadTombstonedAndDisabled(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	f.conn("conn_b", "disabled")
	obj := f.obj("obj_t", "conn_a", source.AuthorizationCollection)
	alice := f.user("alice")
	f.collection("c", "", true, alice)
	f.item("c", obj.ID, false)
	if err := f.repo.TombstoneObject(f.ctx, f.tenant, obj.ID); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	obj, _ = f.repo.GetObject(f.ctx, f.tenant, obj.ID)
	a := source.NewAuthorizer(f.repo)
	mustDeny(t, a.CanRead(f.ctx, f.principal("alice"), obj, source.ReadOptions{}))

	disabled := f.obj("obj_x", "conn_b", source.AuthorizationCollection)
	f.item("c", disabled.ID, false)
	mustDeny(t, a.CanRead(f.ctx, f.principal("alice"), disabled, source.ReadOptions{}))
}

// TestUnknownForbiddenIdentical proves unknown and forbidden objects produce
// the same public outcome.
func TestUnknownForbiddenIdentical(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	f.obj("obj_priv", "conn_a", source.AuthorizationCollection)

	_, unknownErr := f.repo.GetObject(f.ctx, f.tenant, "does-not-exist")
	obj, _ := f.repo.GetObject(f.ctx, f.tenant, "obj_priv")
	forbiddenErr := source.NewAuthorizer(f.repo).
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

	a := source.NewAuthorizer(f.repo)
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

func TestReadTokens(t *testing.T) {
	f := newFixture(t)
	a := source.NewAuthorizer(f.repo)
	owner := source.Object{TenantID: f.tenant, ID: "o", AuthorizationMode: source.AuthorizationOwner, OwnerActorID: "actor-x"}
	tokens, err := a.ReadTokens(f.ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	tokensEqual(t, tokens, "actor:actor-x")

	coll := source.Object{TenantID: f.tenant, ID: "o", AuthorizationMode: source.AuthorizationCollection}
	tokens, err = a.ReadTokens(f.ctx, coll)
	if err != nil {
		t.Fatal(err)
	}
	tokensEqual(t, tokens, "tenant")

	// Ownerless owner-mode and dead objects project the sentinel, never [].
	dead := source.Object{TenantID: f.tenant, ID: "o", AuthorizationMode: source.AuthorizationOwner}
	tokens, err = a.ReadTokens(f.ctx, dead)
	if err != nil {
		t.Fatal(err)
	}
	tokensEqual(t, tokens, source.TokenNone)
	now := time.Now()
	coll.TombstonedAt = &now
	tokens, err = a.ReadTokens(f.ctx, coll)
	if err != nil {
		t.Fatal(err)
	}
	tokensEqual(t, tokens, source.TokenNone)
}

func TestQueryTokens(t *testing.T) {
	f := newFixture(t)
	a := source.NewAuthorizer(f.repo)
	p := f.principal("alice")
	tokens, err := a.QueryTokens(f.ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	tokensEqual(t, tokens, "tenant", "actor:"+p.ActorID)

	// Delegation adds the delegating actor's token.
	p.DelegatedByActorID = "delegator"
	tokens, err = a.QueryTokens(f.ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	tokensEqual(t, tokens, "tenant", "actor:"+p.ActorID, "actor:delegator")
}
