package source_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
)

func (f *fixture) objWith(o source.Object) source.Object {
	f.t.Helper()
	o.TenantID = f.tenant
	if o.ObservedSeq == 0 {
		o.ObservedSeq = 1
	}
	if o.NativeID == "" {
		o.NativeID = "native-" + o.ID
	}
	if o.Kind == "" {
		o.Kind = "repo"
	}
	out, err := f.repo.UpsertObject(f.ctx, o)
	if err != nil {
		f.t.Fatalf("obj %s: %v", o.ID, err)
	}
	return out
}

// TestCanReadManyEquivalent builds one fixture covering every CanRead
// decision path and asserts the batch outcome equals the per-object outcome
// for each object — both the error's nil-ness and its exact value.
func TestCanReadManyEquivalent(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	f.conn("conn_b", "disabled")
	alice := f.user("alice")
	owner := f.user("owner")

	readable := f.objWith(source.Object{ID: "obj_r", ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationCollection})
	f.collection("c_r", "", true, alice)
	f.item("c_r", readable.ID, false)

	twoColls := f.objWith(source.Object{ID: "obj_2c", ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationCollection})
	f.collection("c_hidden", "", true)
	f.collection("c_open", "", true, alice)
	f.item("c_hidden", twoColls.ID, false)
	f.item("c_open", twoColls.ID, false)

	ownerObj := f.objWith(source.Object{ID: "obj_own", ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationOwner, OwnerActorID: owner})
	deadObj := f.objWith(source.Object{ID: "obj_dead", ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationCollection})
	if err := f.repo.TombstoneObject(f.ctx, f.tenant, deadObj.ID); err != nil {
		f.t.Fatalf("tombstone: %v", err)
	}
	deadObj, err := f.repo.GetObject(f.ctx, f.tenant, deadObj.ID)
	if err != nil {
		f.t.Fatalf("reload tombstoned: %v", err)
	}
	f.item("c_r", deadObj.ID, false)
	disabled := f.objWith(source.Object{ID: "obj_dis", ConnectionID: "conn_b", AuthorizationMode: source.AuthorizationCollection})
	f.item("c_r", disabled.ID, false)
	inactiveColl := f.objWith(source.Object{ID: "obj_inact", ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationCollection})
	f.collection("c_off", "", false, alice)
	f.item("c_off", inactiveColl.ID, false)
	noColl := f.objWith(source.Object{ID: "obj_none", ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationCollection})
	removedItem := f.objWith(source.Object{ID: "obj_rm", ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationCollection})
	f.item("c_r", removedItem.ID, true)

	objs := []source.Object{readable, twoColls, ownerObj, deadObj, disabled, inactiveColl, noColl, removedItem}
	a := source.NewAuthorizer(f.repo)

	for _, tc := range []struct {
		name string
		p    auth.Principal
	}{
		{"alice", f.principal("alice")},
		{"owner", f.principal("owner")},
		{"admin", f.admin()},
	} {
		single := make([]error, len(objs))
		for i, obj := range objs {
			single[i] = a.CanRead(f.ctx, tc.p, obj, source.ReadOptions{})
		}
		batch := a.CanReadMany(f.ctx, tc.p, objs, source.ReadOptions{})
		if len(batch) != len(objs) {
			t.Fatalf("%s: batch len %d, want %d", tc.name, len(batch), len(objs))
		}
		for i := range objs {
			if (single[i] == nil) != (batch[i] == nil) ||
				(single[i] != nil && !errors.Is(batch[i], single[i])) {
				t.Fatalf("%s obj %s: single=%v batch=%v", tc.name, objs[i].ID, single[i], batch[i])
			}
		}
		// Spot-check the interesting outcomes: every object after the two
		// readable ones denies for alice.
		if tc.name == "alice" {
			for i, want := range map[int]bool{0: false, 1: false, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true} {
				if (batch[i] != nil) != want {
					t.Fatalf("alice obj %s: denied=%v, want denied=%v", objs[i].ID, batch[i] != nil, want)
				}
			}
		}
	}
}

// countingRepo wraps the postgres SourceRepo to make the batch's query bound
// observable.
type countingRepo struct {
	*postgres.SourceRepo
	calls map[string]int
}

func (r *countingRepo) GetConnection(ctx context.Context, tenantID, id string) (source.Connection, error) {
	r.calls["GetConnection"]++
	return r.SourceRepo.GetConnection(ctx, tenantID, id)
}

func (r *countingRepo) CollectionsForObjects(ctx context.Context, tenantID string, objectIDs []string) (map[string][]source.CollectionAccess, error) {
	r.calls["CollectionsForObjects"]++
	return r.SourceRepo.CollectionsForObjects(ctx, tenantID, objectIDs)
}

// TestCanReadManyBoundedRepoCalls: one batch over many collection objects in
// one tenant costs one collection query plus one connection fetch, not a
// read chain per object.
func TestCanReadManyBoundedRepoCalls(t *testing.T) {
	f := newFixture(t)
	f.conn("conn_a", "active")
	alice := f.user("alice")
	f.collection("c_bulk", "", true, alice)

	objects := make([]source.Object, 0, 30)
	for i := range 30 {
		obj := f.objWith(source.Object{ID: fmt.Sprintf("obj_bulk_%02d", i), ConnectionID: "conn_a", AuthorizationMode: source.AuthorizationCollection})
		f.item("c_bulk", obj.ID, false)
		objects = append(objects, obj)
	}

	repo := &countingRepo{SourceRepo: f.repo, calls: map[string]int{}}
	a := source.NewAuthorizer(repo)
	errs := a.CanReadMany(f.ctx, f.principal("alice"), objects, source.ReadOptions{})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("object %d denied: %v", i, err)
		}
	}
	if repo.calls["CollectionsForObjects"] != 1 || repo.calls["GetConnection"] != 1 {
		t.Fatalf("CanReadMany repo calls = %+v, want one collection and one connection fetch", repo.calls)
	}
}
