package source

import (
	"context"

	"enclava-go/internal/auth"
)

// connKey scopes batched lookups exactly like the single-read path does:
// every repo read carries both tenant and connection.
type connKey struct {
	tenantID     string
	connectionID string
}

// CanReadMany decides, per object, what CanRead would return for the same
// (principal, object, opts) — result i is nil or the same error CanRead
// produces for objs[i]. It shares every check and evaluation with the
// single-read path and differs only in batching the repo reads: a constant
// number of queries per (tenant, connection) instead of per object.
func (a *Authorizer) CanReadMany(ctx context.Context, p auth.Principal, objs []Object, opts ReadOptions) []error {
	errs := make([]error, len(objs))
	pending := make([]int, 0, len(objs))
	// Tombstoned/deleted objects deny identically to unknown objects.
	for i, obj := range objs {
		if obj.DeletedAt != nil || obj.TombstonedAt != nil {
			errs[i] = ErrDenied
			continue
		}
		pending = append(pending, i)
	}
	// Connector objects on a disabled/deleted connection deny; one fetch per
	// (tenant, connection) instead of one per object.
	active := make(map[connKey]bool)
	for _, i := range pending {
		obj := objs[i]
		if obj.ConnectionID == "" {
			continue
		}
		key := connKey{obj.TenantID, obj.ConnectionID}
		if _, checked := active[key]; checked {
			continue
		}
		conn, err := a.repo.GetConnection(ctx, obj.TenantID, obj.ConnectionID)
		active[key] = err == nil && conn.Status == "active"
	}
	// Request options may narrow, never widen, persisted restrictions;
	// checkNarrowing only queries when opts.RequestedRestriction is set.
	stillPending := pending[:0]
	for _, i := range pending {
		obj := objs[i]
		if obj.ConnectionID != "" && !active[connKey{obj.TenantID, obj.ConnectionID}] {
			errs[i] = ErrDenied
			continue
		}
		if err := a.checkNarrowing(ctx, obj, opts.RequestedRestriction); err != nil {
			errs[i] = err
			continue
		}
		stillPending = append(stillPending, i)
	}
	// Owner mode evaluates in memory; collection-mode objects batch their
	// containing-collection read below.
	var coll []int
	for _, i := range stillPending {
		obj := objs[i]
		switch obj.AuthorizationMode {
		case AuthorizationOwner:
			if obj.OwnerActorID == p.ActorID ||
				(p.DelegatedByActorID != "" && obj.OwnerActorID == p.DelegatedByActorID) {
				continue
			}
			errs[i] = ErrDenied
		case AuthorizationCollection:
			coll = append(coll, i)
		default:
			errs[i] = ErrDenied
		}
	}
	if len(coll) == 0 {
		return errs
	}
	// Group by tenant — collections are tenant-scoped.
	byTenant := make(map[string][]int)
	for _, i := range coll {
		byTenant[objs[i].TenantID] = append(byTenant[objs[i].TenantID], i)
	}
	for tenantID, idxs := range byTenant {
		objectIDs := make([]string, 0, len(idxs))
		for _, i := range idxs {
			objectIDs = append(objectIDs, objs[i].ID)
		}
		collections, err := a.repo.CollectionsForObjects(ctx, tenantID, objectIDs)
		if err != nil {
			for _, i := range idxs {
				errs[i] = err
			}
			continue
		}
		for _, i := range idxs {
			readable := false
			for _, c := range collections[objs[i].ID] {
				if CollectionReadable(c, p) {
					readable = true
					break
				}
			}
			if !readable {
				errs[i] = ErrDenied
			}
		}
	}
	return errs
}
