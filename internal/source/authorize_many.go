package source

import (
	"context"
	"errors"
	"time"

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
	// Owner/audience modes evaluate in memory; source-ACL objects batch their
	// connection reads below.
	var acl []int
	for _, i := range stillPending {
		obj := objs[i]
		switch obj.AuthorizationMode {
		case AuthorizationOwner:
			if obj.OwnerActorID == p.ActorID ||
				(p.DelegatedByActorID != "" && obj.OwnerActorID == p.DelegatedByActorID) {
				continue
			}
			errs[i] = ErrDenied
		case AuthorizationAudience, AuthorizationPublishedAudience:
			// Published audiences are explicit publisher-assigned Enclava
			// audiences; upstream access loss arrives via object/connection
			// status, handled above.
			if contains(obj.Audience, p.ActorID) || contains(obj.Audience, "*") {
				continue
			}
			errs[i] = ErrDenied
		case AuthorizationSourceACL:
			acl = append(acl, i)
		default:
			errs[i] = ErrDenied
		}
	}
	a.canReadSourceACLBatch(ctx, p, objs, acl, opts, errs)
	return errs
}

// canReadSourceACLBatch resolves every SourceACL object in idxs with one
// revision/identity/grants pass per (tenant, connection): LatestComplete-
// Revision, AncestorObjectIDsBatch and GrantsForObjects fetch once for the
// whole object set, then each object evaluates its own ancestor chain
// through the shared evalSourceACLGrants. Membership is read once per
// connection and only when an applicable group grant and a mapped identity
// exist — matching canReadSourceACL's conditional fetch.
func (a *Authorizer) canReadSourceACLBatch(ctx context.Context, p auth.Principal, objs []Object, idxs []int, opts ReadOptions, errs []error) {
	byConn := make(map[connKey][]int)
	for _, i := range idxs {
		obj := objs[i]
		if obj.ConnectionID == "" {
			errs[i] = ErrDenied
			continue
		}
		key := connKey{obj.TenantID, obj.ConnectionID}
		byConn[key] = append(byConn[key], i)
	}
	mappedUser := p.UserID
	if mappedUser == "" {
		mappedUser = p.ActorID
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	for key, group := range byConn {
		a.canReadSourceACLConn(ctx, key, mappedUser, now, objs, group, errs)
	}
}

// canReadSourceACLConn is the per-(tenant, connection) batch pass. Every
// deny it assigns reproduces the exact ErrDenied canReadSourceACL returns
// for the object evaluated alone.
func (a *Authorizer) canReadSourceACLConn(ctx context.Context, key connKey, mappedUser string, now time.Time, objs []Object, group []int, errs []error) {
	rev, completedAt, err := a.repo.LatestCompleteRevision(ctx, key.tenantID, key.connectionID, SnapshotGrants)
	if err != nil || completedAt.IsZero() || completedAt.After(now) || (a.MaxStale > 0 && now.Sub(completedAt) > a.MaxStale) {
		denyAll(errs, group)
		return
	}
	objectIDs := make([]string, 0, len(group))
	for _, i := range group {
		objectIDs = append(objectIDs, objs[i].ID)
	}
	chains, err := a.repo.AncestorObjectIDsBatch(ctx, key.tenantID, key.connectionID, objectIDs, 8)
	if err != nil {
		denyAll(errs, group)
		return
	}
	chainByIndex := make(map[int][]string, len(group))
	union := []string{}
	seenID := make(map[string]struct{})
	var pending []int
	for _, i := range group {
		chain, ok := chains[objs[i].ID]
		if !ok {
			errs[i] = ErrDenied
			continue
		}
		chainByIndex[i] = chain
		for _, id := range chain {
			if _, dup := seenID[id]; !dup {
				seenID[id] = struct{}{}
				union = append(union, id)
			}
		}
		pending = append(pending, i)
	}
	if len(pending) == 0 {
		return
	}
	grants, err := a.repo.GrantsForObjects(ctx, key.tenantID, key.connectionID, union, rev)
	if err != nil {
		denyAll(errs, pending)
		return
	}
	byObject := make(map[string][]SourceGrant, len(union))
	for _, g := range grants {
		byObject[g.ObjectID] = append(byObject[g.ObjectID], g)
	}
	ident, err := a.repo.SourceIdentityForUser(ctx, key.tenantID, key.connectionID, mappedUser)
	if err != nil && !errors.Is(err, ErrDenied) {
		denyAll(errs, pending)
		return
	}
	grantsFor := func(i int) []SourceGrant {
		var out []SourceGrant
		for _, id := range chainByIndex[i] {
			out = append(out, byObject[id]...)
		}
		return out
	}
	// Membership is fetched only when some object has an applicable group
	// grant and a mapped identity exists — the same condition the single
	// read applies, lifted to the connection's whole object set.
	needMembership := false
	if ident.ID != "" {
		for _, i := range pending {
			for _, g := range grantsFor(i) {
				if g.SubjectKind == GrantSubjectGroup {
					needMembership = true
					break
				}
			}
			if needMembership {
				break
			}
		}
	}
	var inGroup map[string]struct{}
	membershipKnown := false
	if needMembership {
		inGroup = map[string]struct{}{}
		membershipRev, membershipAt, err := a.repo.LatestCompleteRevision(ctx, key.tenantID, key.connectionID, SnapshotMembership)
		if err == nil && !membershipAt.IsZero() && !membershipAt.After(now) && (a.MaxStale <= 0 || now.Sub(membershipAt) <= a.MaxStale) {
			groups, gerr := a.repo.GroupIDsForIdentity(ctx, key.tenantID, key.connectionID, ident.ID, membershipRev)
			if membershipKnown = gerr == nil; membershipKnown {
				for _, g := range groups {
					inGroup[g] = struct{}{}
				}
			}
		}
	}
	for _, i := range pending {
		errs[i] = evalSourceACLGrants(ident.ID, inGroup, membershipKnown, grantsFor(i))
	}
}

func denyAll(errs []error, idxs []int) {
	for _, i := range idxs {
		errs[i] = ErrDenied
	}
}
