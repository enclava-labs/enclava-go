package source

import (
	"context"
	"errors"
	"time"

	"enclava-go/internal/auth"
)

// Authorizer is the single fail-closed decision point every source read must
// pass before exposing titles, metadata, links or content (AUTH-05/AUTH-06).
// Unknown, forbidden, stale-grant and tombstoned inputs all return ErrDenied —
// callers cannot distinguish them.
type Authorizer struct {
	repo SourceRepository
	// MaxStale bounds the age of complete grant and membership snapshots.
	// Zero means unbounded.
	MaxStale time.Duration
}

func NewAuthorizer(repo SourceRepository, maxStale time.Duration) *Authorizer {
	return &Authorizer{repo: repo, MaxStale: maxStale}
}

// CanRead decides whether principal p may read obj under opts. Denial is
// always ErrDenied.
func (a *Authorizer) CanRead(ctx context.Context, p auth.Principal, obj Object, opts ReadOptions) error {
	// Tombstoned/deleted objects deny identically to unknown objects.
	if obj.DeletedAt != nil || obj.TombstonedAt != nil {
		return ErrDenied
	}
	// Connector objects on a disabled/deleted connection deny.
	if obj.ConnectionID != "" {
		conn, err := a.repo.GetConnection(ctx, obj.TenantID, obj.ConnectionID)
		if err != nil || conn.Status != "active" {
			return ErrDenied
		}
	}
	// Request options may narrow, never widen, persisted restrictions.
	if err := a.checkNarrowing(ctx, obj, opts.RequestedRestriction); err != nil {
		return err
	}

	switch obj.AuthorizationMode {
	case AuthorizationOwner:
		if obj.OwnerActorID == p.ActorID ||
			(p.DelegatedByActorID != "" && obj.OwnerActorID == p.DelegatedByActorID) {
			return nil
		}
		return ErrDenied
	case AuthorizationAudience, AuthorizationPublishedAudience:
		// Published audiences are explicit publisher-assigned Enclava
		// audiences; upstream access loss arrives via object/connection
		// status, handled above.
		if contains(obj.Audience, p.ActorID) || contains(obj.Audience, "*") {
			return nil
		}
		return ErrDenied
	case AuthorizationSourceACL:
		return a.canReadSourceACL(ctx, p, obj, opts)
	default:
		return ErrDenied
	}
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// canReadSourceACL evaluates effective source grants at the latest complete,
// non-stale grants snapshot. DENY WINS FIRST: any applicable deny grant for
// the mapped source identity or a group containing it denies outright, even
// when an allow grant also exists.
func (a *Authorizer) canReadSourceACL(ctx context.Context, p auth.Principal, obj Object, opts ReadOptions) error {
	if obj.ConnectionID == "" {
		return ErrDenied
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	rev, completedAt, err := a.repo.LatestCompleteRevision(ctx, obj.TenantID, obj.ConnectionID, SnapshotGrants)
	if err != nil || completedAt.IsZero() || completedAt.After(now) || (a.MaxStale > 0 && now.Sub(completedAt) > a.MaxStale) {
		return ErrDenied
	}
	objectIDs, err := a.repo.AncestorObjectIDs(ctx, obj.TenantID, obj.ConnectionID, obj.ID, 8)
	if err != nil {
		return ErrDenied
	}
	grants, err := a.repo.GrantsForObjects(ctx, obj.TenantID, obj.ConnectionID, objectIDs, rev)
	if err != nil {
		return ErrDenied
	}
	mappedUser := p.UserID
	if mappedUser == "" {
		mappedUser = p.ActorID
	}
	ident, err := a.repo.SourceIdentityForUser(ctx, obj.TenantID, obj.ConnectionID, mappedUser)
	if err != nil && !errors.Is(err, ErrDenied) {
		return ErrDenied
	}
	hasGroups := false
	for _, g := range grants {
		if g.SubjectKind == GrantSubjectGroup {
			hasGroups = true
			break
		}
	}
	var groups []string
	membershipKnown := false
	if hasGroups && ident.ID != "" {
		membershipRev, membershipAt, err := a.repo.LatestCompleteRevision(ctx, obj.TenantID, obj.ConnectionID, SnapshotMembership)
		if err == nil && !membershipAt.IsZero() && !membershipAt.After(now) && (a.MaxStale <= 0 || now.Sub(membershipAt) <= a.MaxStale) {
			groups, err = a.repo.GroupIDsForIdentity(ctx, obj.TenantID, obj.ConnectionID, ident.ID, membershipRev)
			membershipKnown = err == nil
		}
	}
	inGroup := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		inGroup[group] = struct{}{}
	}
	allowed := false
	for _, g := range grants {
		var applies bool
		switch g.SubjectKind {
		case GrantSubjectAll:
			applies = true
		case GrantSubjectUser:
			applies = ident.ID != "" && g.SubjectID == ident.ID
		case GrantSubjectGroup:
			if !membershipKnown {
				if g.Effect == GrantEffectDeny {
					return ErrDenied
				}
				continue
			}
			_, applies = inGroup[g.SubjectID]
		}
		if !applies {
			continue
		}
		if g.Effect == GrantEffectDeny {
			return ErrDenied
		}
		if g.Effect == GrantEffectAllow {
			allowed = true
		}
	}
	if !allowed {
		return ErrDenied
	}
	return nil
}

// CanReadVersion additionally verifies the version exists and inherits the
// object's authorization state.
func (a *Authorizer) CanReadVersion(ctx context.Context, p auth.Principal, obj Object, ver ObjectVersion, opts ReadOptions) error {
	if ver.ObjectID != obj.ID {
		return ErrDenied
	}
	if _, err := a.repo.GetObjectVersion(ctx, obj.TenantID, obj.ID, ver.ObservedSeq); err != nil {
		return ErrDenied
	}
	return a.CanRead(ctx, p, obj, opts)
}

// assuranceRank orders required assurance; unknown values rank highest so an
// unrecognized persisted level can never be weakened by a request.
func assuranceRank(level string) int {
	switch level {
	case "":
		return 0
	case "standard":
		return 1
	case "high":
		return 2
	default:
		return 3
	}
}

func mergeRestriction(dst *SourceRestriction, r SourceRestriction) {
	if assuranceRank(r.RequiredAssurance) > assuranceRank(dst.RequiredAssurance) {
		dst.RequiredAssurance = r.RequiredAssurance
	}
	dst.DeniedCapabilities = unionStrings(dst.DeniedCapabilities, r.DeniedCapabilities)
	dst.PolicyLabels = unionStrings(dst.PolicyLabels, r.PolicyLabels)
}

func unionStrings(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := append([]string{}, a...)
	for _, v := range a {
		seen[v] = struct{}{}
	}
	for _, v := range b {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

// EffectiveRestrictions returns the union of the connection, object,
// version-embedded and (optional) artifact restrictions that apply to this
// read. Derivatives inherit it; request options can only tighten it.
func (a *Authorizer) EffectiveRestrictions(ctx context.Context, obj Object, ver *ObjectVersion, artifactID string) (SourceRestriction, error) {
	merged := SourceRestriction{TenantID: obj.TenantID}
	subjects := [][2]string{}
	if obj.ConnectionID != "" {
		subjects = append(subjects, [2]string{RestrictionConnection, obj.ConnectionID})
	}
	subjects = append(subjects, [2]string{RestrictionObject, obj.ID})
	if artifactID != "" {
		subjects = append(subjects, [2]string{RestrictionArtifact, artifactID})
	}
	for _, s := range subjects {
		rows, err := a.repo.RestrictionsForSubject(ctx, obj.TenantID, s[0], s[1])
		if err != nil {
			return SourceRestriction{}, err
		}
		for _, r := range rows {
			mergeRestriction(&merged, r)
		}
	}
	if ver != nil {
		for _, r := range ver.Restrictions {
			mergeRestriction(&merged, r)
		}
	}
	return merged, nil
}

// checkNarrowing rejects a request-provided restriction set that would widen
// the effective policy: every persisted denied capability and the persisted
// assurance floor must be retained. Only evaluated when a request supplies
// narrowing options.
func (a *Authorizer) checkNarrowing(ctx context.Context, obj Object, requested *SourceRestriction) error {
	if requested == nil {
		return nil
	}
	persisted, err := a.EffectiveRestrictions(ctx, obj, nil, "")
	if err != nil {
		return err
	}
	reqDenied := make(map[string]struct{}, len(requested.DeniedCapabilities))
	for _, c := range requested.DeniedCapabilities {
		reqDenied[c] = struct{}{}
	}
	for _, c := range persisted.DeniedCapabilities {
		if _, ok := reqDenied[c]; !ok {
			return ErrDenied // dropping a persisted deny would widen access
		}
	}
	if assuranceRank(requested.RequiredAssurance) < assuranceRank(persisted.RequiredAssurance) {
		return ErrDenied
	}
	return nil
}
