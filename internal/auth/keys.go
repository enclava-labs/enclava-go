package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// IssueKeyInput describes an administrator's API key grant.
type IssueKeyInput struct {
	Name               string
	OwnerUserID        string // exactly one of OwnerUserID / ServiceAccountID
	ServiceAccountID   string
	Scopes             []string
	AllowedModels      Allowlist
	AllowedTemplates   Allowlist
	AllowedChatbots    Allowlist
	AllowedAgents      Allowlist
	AllowedEndpoints   Allowlist
	AllowedIPs         Allowlist
	RateLimitPerMinute *int
	RateLimitPerHour   *int
	RateLimitPerDay    *int
	ExpiresAt          *time.Time
}

// Allowlist is a stored grant: All=true encodes {"mode":"all"} (the only
// unrestricted form); otherwise Items encode {"mode":"list","items":[...]}.
type Allowlist struct {
	All   bool
	Items []string
}

func (a Allowlist) marshal() json.RawMessage {
	if a.All {
		return json.RawMessage(`{"mode":"all"}`)
	}
	raw, err := json.Marshal(struct {
		Mode  string   `json:"mode"`
		Items []string `json:"items"`
	}{Mode: "list", Items: a.Items})
	if err != nil {
		return json.RawMessage(`{"mode":"list","items":[]}`)
	}
	return raw
}

func scopeList(scopes []string) json.RawMessage {
	return Allowlist{Items: scopes}.marshal()
}

// IssueAPIKey creates a stored tenant-scoped key for an administrator actor
// and returns the plaintext secret exactly once. HashAlgo/pepper handling
// matches Authenticate (HashForStorage).
func (s *Service) IssueAPIKey(ctx context.Context, admin Principal, in IssueKeyInput) (plaintext string, record APIKeyRecord, err error) {
	if !HasScope(admin, ScopePlatformAdmin) {
		return "", APIKeyRecord{}, errors.New("platform.admin scope required to issue keys")
	}
	if admin.TenantID == "" {
		return "", APIKeyRecord{}, errors.New("admin principal has no tenant")
	}
	if (in.OwnerUserID == "") == (in.ServiceAccountID == "") {
		return "", APIKeyRecord{}, errors.New("exactly one of owner_user_id or service_account_id is required")
	}
	plaintext = "enc_" + randomHex(24)
	prefix, hash, algo, err := HashForStorage(plaintext, HashAlgoHMACSHA256, string(s.pepper))
	if err != nil {
		if len(s.pepper) == 0 {
			// Pepper-less deployments hash with plain sha256.
			prefix, hash, algo, err = HashForStorage(plaintext, HashAlgoSHA256, "")
		}
		if err != nil {
			return "", APIKeyRecord{}, err
		}
	}
	record = APIKeyRecord{
		ID:                 "key_" + randomHex(12),
		TenantID:           admin.TenantID,
		OwnerUserID:        in.OwnerUserID,
		ServiceAccountID:   in.ServiceAccountID,
		Name:               in.Name,
		KeyPrefix:          prefix,
		KeyHash:            hash,
		KeyHashAlgo:        algo,
		Scopes:             scopeList(in.Scopes),
		AllowedModels:      in.AllowedModels.marshal(),
		AllowedTemplates:   in.AllowedTemplates.marshal(),
		AllowedChatbots:    in.AllowedChatbots.marshal(),
		AllowedAgents:      in.AllowedAgents.marshal(),
		AllowedEndpoints:   in.AllowedEndpoints.marshal(),
		AllowedIPs:         in.AllowedIPs.marshal(),
		RateLimitPerMinute: in.RateLimitPerMinute,
		RateLimitPerHour:   in.RateLimitPerHour,
		RateLimitPerDay:    in.RateLimitPerDay,
		IsActive:           true,
		ExpiresAt:          in.ExpiresAt,
	}
	if err := s.repo.CreateAPIKey(ctx, admin.TenantID, record); err != nil {
		return "", APIKeyRecord{}, err
	}
	return plaintext, record, nil
}

// RevokeAPIKey immediately denies the key's next Authenticate call.
func (s *Service) RevokeAPIKey(ctx context.Context, admin Principal, keyID string) error {
	if !HasScope(admin, ScopePlatformAdmin) {
		return errors.New("platform.admin scope required to revoke keys")
	}
	return s.repo.RevokeAPIKey(ctx, admin.TenantID, keyID, time.Now().UTC())
}

// ValidateDelegation checks the stored delegation for (delegator, delegate):
// not expired, not revoked, effective scopes ⊆ allowed_scopes ⊆ delegator's
// role scopes. The returned Principal carries DelegatedByActorID and the
// delegator's billing subject.
func (s *Service) ValidateDelegation(ctx context.Context, delegate Principal, delegatorUserID string) (Principal, error) {
	if delegate.ActorKind != ActorKindServiceAccount || delegate.ActorID == "" {
		return Principal{}, errors.New("delegation requires a service-account principal")
	}
	delegation, err := s.repo.FindDelegation(ctx, delegate.TenantID, delegatorUserID, delegate.ActorID)
	if err != nil {
		return Principal{}, fmt.Errorf("no delegation for delegate: %w", err)
	}
	now := time.Now().UTC()
	if delegation.RevokedAt != nil {
		return Principal{}, errors.New("delegation is revoked")
	}
	if now.After(delegation.ExpiresAt) {
		return Principal{}, errors.New("delegation is expired")
	}
	allowed, all := DecodeAllowlist(delegation.AllowedScopes)
	if all {
		return Principal{}, errors.New("delegation allowed_scopes must be an explicit list")
	}
	delegator, err := s.repo.GetUser(ctx, delegate.TenantID, delegatorUserID)
	if err != nil {
		return Principal{}, fmt.Errorf("resolve delegator: %w", err)
	}
	if delegator.Status != "active" {
		return Principal{}, errors.New("delegator is disabled")
	}
	delegatorPrincipal, err := s.principalForUser(ctx, delegator)
	if err != nil {
		return Principal{}, err
	}
	// allowed_scopes must not exceed what the delegator holds.
	for scope := range allowed {
		if _, ok := delegatorPrincipal.Scopes[scope]; !ok {
			return Principal{}, fmt.Errorf("delegation scope %q exceeds delegator scopes", scope)
		}
	}
	// The derived principal may exercise only delegate ∩ allowed scopes.
	effective := make(map[string]struct{})
	for scope := range delegate.Scopes {
		if _, ok := allowed[scope]; ok {
			effective[scope] = struct{}{}
		}
	}
	out := delegate
	out.Scopes = effective
	out.DelegatedByActorID = delegatorUserID
	out.BillingSubjectID = delegator.ID
	out.BillingSubjectKind = BillingSubjectUser
	return out, nil
}
