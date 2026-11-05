package source

import (
	"context"
	"errors"

	"enclava-go/internal/auth"
)

// Authorizer is the single fail-closed decision point every source read must
// pass before exposing titles, metadata, links or content (AUTH-05/AUTH-06).
// Unknown, forbidden and tombstoned inputs all return ErrDenied — callers
// cannot distinguish them.
type Authorizer struct {
	repo SourceRepository
}

func NewAuthorizer(repo SourceRepository) *Authorizer {
	return &Authorizer{repo: repo}
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
	case AuthorizationCollection:
		collections, err := a.repo.CollectionsForObjects(ctx, obj.TenantID, []string{obj.ID})
		if err != nil {
			return err
		}
		for _, c := range collections[obj.ID] {
			if CollectionReadable(c, p) {
				return nil
			}
		}
		return ErrDenied
	default:
		return ErrDenied
	}
}

// CollectionReadable is THE collection read rule: platform admins read
// everything; other principals read an active collection they own or whose
// audience lists their actor id or "*". The Authorizer applies it to every
// collection containing an object; collection APIs apply it directly.
func CollectionReadable(access CollectionAccess, p auth.Principal) bool {
	if auth.HasScope(p, "platform.admin") {
		return true
	}
	if !access.IsActive {
		return false
	}
	if access.OwnerActorID != "" && access.OwnerActorID == p.ActorID {
		return true
	}
	for _, aud := range access.Audience {
		if aud == "*" || aud == p.ActorID {
			return true
		}
	}
	return false
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

// ---------------------------------------------------------------------
// Search access projection: the prefilter tokens stamped on index points
// and the token set a principal matches at query time. Tokens mirror the
// ALLOW side of CanRead exactly; denies are never projected (the read-time
// PostgreSQL post-check enforces them), so a token may over-include but the
// post-check keeps results exact.

// TokenNone is the sentinel ACL for an object nobody can read — a point
// always carries a non-empty acl array so "no acl" means only "not yet
// projected".
const TokenNone = "none"

// ReadTokens computes allow-path tokens. Collection-mode objects project
// only "tenant": the collection scoping plus the authoritative read-time
// post-check enforce access. Disabled connections retain their tokens so
// re-enabling them does not require re-projection; connection status is
// enforced by the read-time post-check.
func (a *Authorizer) ReadTokens(ctx context.Context, obj Object) ([]string, error) {
	if obj.DeletedAt != nil || obj.TombstonedAt != nil {
		return []string{TokenNone}, nil
	}
	if obj.ConnectionID != "" {
		_, err := a.repo.GetConnection(ctx, obj.TenantID, obj.ConnectionID)
		if err != nil {
			if errors.Is(err, ErrDenied) {
				return []string{TokenNone}, nil
			}
			return nil, err
		}
	}
	switch obj.AuthorizationMode {
	case AuthorizationOwner:
		if obj.OwnerActorID == "" {
			return []string{TokenNone}, nil
		}
		return []string{"actor:" + obj.OwnerActorID}, nil
	case AuthorizationCollection:
		return []string{"tenant"}, nil
	default:
		return []string{TokenNone}, nil
	}
}

// QueryTokens computes the prefilter token set one principal matches:
// "tenant", "actor:<ActorID>" and "actor:<DelegatedByActorID>" when
// delegated. No repo access is needed — collection scoping and the
// read-time post-check do the filtering.
func (a *Authorizer) QueryTokens(_ context.Context, p auth.Principal) ([]string, error) {
	tokens := []string{"tenant"}
	if p.ActorID != "" {
		tokens = append(tokens, "actor:"+p.ActorID)
	}
	if p.DelegatedByActorID != "" {
		tokens = append(tokens, "actor:"+p.DelegatedByActorID)
	}
	return tokens, nil
}
