package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"time"
)

const (
	HashAlgoSHA256     = "sha256"
	HashAlgoHMACSHA256 = "hmac_sha256"
)

// APIKeyRecord is a stored tenant-scoped API key row (postgres api_keys).
// Allowlist columns are stored grant documents decoded via DecodeAllowlist:
// {"mode":"all"} is the only unrestricted grant, {"mode":"list","items":[...]}
// enumerates, absent/malformed denies everything.
type APIKeyRecord struct {
	ID                 string
	TenantID           string
	OwnerUserID        string
	ServiceAccountID   string
	Name               string
	KeyPrefix          string
	KeyHash            string
	KeyHashAlgo        string
	Scopes             json.RawMessage
	AllowedModels      json.RawMessage
	AllowedTemplates   json.RawMessage
	AllowedChatbots    json.RawMessage
	AllowedAgents      json.RawMessage
	AllowedEndpoints   json.RawMessage
	AllowedIPs         json.RawMessage
	RateLimitPerMinute *int
	RateLimitPerHour   *int
	RateLimitPerDay    *int
	IsActive           bool
	ExpiresAt          *time.Time
	RevokedAt          *time.Time
	LastUsedAt         *time.Time
	CreatedAt          time.Time

	// Legacy fields retained on the record only so the retained sqlite
	// producer keeps compiling until its port (Principal populates them for
	// postgres rows as nil/zero — the columns do not exist there).
	IsUnlimited       bool
	BudgetLimitTokens *int64
	BudgetLimitCents  *int64
	BudgetPeriod      string
}

type Service struct {
	repo   IdentityRepository
	pepper []byte
}

func NewService(repo IdentityRepository, pepper string) *Service {
	return &Service{repo: repo, pepper: []byte(pepper)}
}

func ParseAPIKey(r *http.Request) string {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		candidate := strings.TrimSpace(authHeader[7:])
		if candidate != "" {
			return candidate
		}
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

var errInvalidCredential = errors.New("invalid credentials")

// Authenticate validates a plaintext API key against stored tenant-scoped
// rows and returns the canonical Principal. The tenant and actor resolve
// strictly from the verified stored row — never from the request.
func (s *Service) Authenticate(ctx context.Context, plaintext string) (Principal, error) {
	if plaintext == "" {
		return Principal{}, errors.New("missing API key")
	}
	if len(plaintext) < 8 {
		return Principal{}, errInvalidCredential
	}
	prefix := plaintext[:8]
	candidates, err := s.repo.FindKeyCandidates(ctx, prefix)
	if err != nil {
		return Principal{}, fmt.Errorf("lookup API key: %w", err)
	}

	hashes := make(map[string]string, 2)
	hashFor := func(algo string) (string, error) {
		if h, ok := hashes[algo]; ok {
			return h, nil
		}
		h, err := s.hashAPIKey(plaintext, algo)
		if err != nil {
			return "", err
		}
		hashes[algo] = h
		return h, nil
	}

	for _, candidate := range candidates {
		algo := strings.TrimSpace(candidate.KeyHashAlgo)
		if algo == "" {
			algo = HashAlgoSHA256
		}
		hash, err := hashFor(algo)
		if err != nil {
			// Unknown/unsupported hash schemes never match.
			continue
		}
		if subtle.ConstantTimeCompare([]byte(hash), []byte(candidate.KeyHash)) != 1 {
			continue
		}
		// Verify usability BEFORE trusting the row's tenant.
		if candidate.RevokedAt != nil || !candidate.IsActive {
			return Principal{}, errors.New("API key is revoked")
		}
		if candidate.ExpiresAt != nil && time.Now().UTC().After(*candidate.ExpiresAt) {
			return Principal{}, errors.New("API key is expired")
		}
		return s.principalFromKey(ctx, candidate)
	}
	return Principal{}, errInvalidCredential
}

func (s *Service) principalFromKey(ctx context.Context, key APIKeyRecord) (Principal, error) {
	tenantID := key.TenantID
	principal := Principal{
		TenantID:        tenantID,
		CredentialID:    key.ID,
		CredentialKind:  CredentialKindAPIKey,
		Name:            key.Name,
		RawAPIKeyPrefix: key.KeyPrefix,
		// Legacy numeric id: deterministic handle derived from the stored
		// credential row so unported callers keep working until 01-35.
		APIKeyID:          LegacyAPIKeyID(tenantID, key.ID),
		IsUnlimited:       key.IsUnlimited,
		BudgetLimitTokens: key.BudgetLimitTokens,
		BudgetLimitCents:  key.BudgetLimitCents,
		BudgetPeriod:      key.BudgetPeriod,

		RateLimitPerMinute: key.RateLimitPerMinute,
		RateLimitPerHour:   key.RateLimitPerHour,
		RateLimitPerDay:    key.RateLimitPerDay,
	}
	principal.Scopes, _ = DecodeAllowlist(key.Scopes)
	principal.AllowedModels, principal.AllowedModelsAll = DecodeAllowlist(key.AllowedModels)
	principal.AllowedExtractTemplates, principal.AllowedExtractTemplatesAll = DecodeAllowlist(key.AllowedTemplates)
	principal.AllowedChatbots, principal.AllowedChatbotsAll = DecodeAllowlist(key.AllowedChatbots)
	principal.AllowedAgents, principal.AllowedAgentsAll = DecodeAllowlist(key.AllowedAgents)
	principal.AllowedEndpoints, principal.AllowedEndpointsAll = DecodeAllowlist(key.AllowedEndpoints)
	principal.AllowedIPs, principal.AllowedIPsAll = DecodeAllowlist(key.AllowedIPs)

	switch {
	case key.OwnerUserID != "":
		user, err := s.repo.GetUser(ctx, tenantID, key.OwnerUserID)
		if err != nil {
			return Principal{}, fmt.Errorf("resolve key owner: %w", err)
		}
		if user.Status != "active" {
			return Principal{}, errors.New("key owner is disabled")
		}
		principal.ActorID = user.ID
		principal.ActorKind = ActorKindUser
		principal.UserID = user.ID
		principal.BillingSubjectID = user.ID
		principal.BillingSubjectKind = BillingSubjectUser
	case key.ServiceAccountID != "":
		sa, err := s.repo.GetServiceAccount(ctx, tenantID, key.ServiceAccountID)
		if err != nil {
			return Principal{}, fmt.Errorf("resolve key service account: %w", err)
		}
		if sa.Status != "active" {
			return Principal{}, errors.New("key service account is disabled")
		}
		principal.ActorID = sa.ID
		principal.ActorKind = ActorKindServiceAccount
		principal.BillingSubjectID = sa.ID
		principal.BillingSubjectKind = BillingSubjectServiceAccount
	default:
		// The schema CHECK guarantees an owner, but a row from a non-conforming
		// store still bills the credential itself — never invents an actor.
		principal.ActorID = key.ID
		principal.ActorKind = ActorKindServiceAccount
		principal.BillingSubjectID = key.ID
		principal.BillingSubjectKind = BillingSubjectAPIKey
	}
	_ = s.repo.TouchAPIKeyUsage(ctx, tenantID, key.ID, time.Now().UTC())
	return principal, nil
}

// LegacyAPIKeyID derives the retained numeric APIKeyID legacy field from the
// stored credential identity. Deterministic per row; retired with the field.
func LegacyAPIKeyID(tenantID, keyID string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(tenantID + "/" + keyID))
	return int64(h.Sum64() & 0x7fffffffffffffff)
}

// AuthenticatePassword verifies a local account's bcrypt password and returns
// the user's principal (password auth precedes session issuance).
func (s *Service) AuthenticatePassword(ctx context.Context, tenantID, email, password string) (Principal, error) {
	user, err := s.repo.GetUserByEmail(ctx, tenantID, strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		return Principal{}, errInvalidCredential
	}
	if !VerifyPassword(user.PasswordHash, password) {
		return Principal{}, errInvalidCredential
	}
	if user.Status != "active" {
		return Principal{}, errors.New("user is disabled")
	}
	_ = s.repo.TouchLastLogin(ctx, tenantID, user.ID, time.Now().UTC())
	return s.principalForUser(ctx, user)
}

// principalForUser builds a principal for a stored, active user: scopes are
// the union of the user's assigned role scopes.
func (s *Service) principalForUser(ctx context.Context, user User) (Principal, error) {
	roles, err := s.repo.RolesForUser(ctx, user.TenantID, user.ID)
	if err != nil {
		return Principal{}, fmt.Errorf("load user roles: %w", err)
	}
	scopes := make(map[string]struct{})
	for _, role := range roles {
		for _, scope := range role.Scopes {
			if scope != "" {
				scopes[scope] = struct{}{}
			}
		}
	}
	return Principal{
		TenantID:           user.TenantID,
		ActorID:            user.ID,
		ActorKind:          ActorKindUser,
		UserID:             user.ID,
		Scopes:             scopes,
		BillingSubjectID:   user.ID,
		BillingSubjectKind: BillingSubjectUser,
	}, nil
}

func HashForStorage(plaintext, algo, pepper string) (prefix string, hash string, usedAlgo string, err error) {
	if len(plaintext) >= 8 {
		prefix = plaintext[:8]
	}
	usedAlgo = strings.TrimSpace(algo)
	if usedAlgo == "" {
		usedAlgo = HashAlgoSHA256
	}
	hash, err = hashAPIKeyWithAlgo(plaintext, usedAlgo, []byte(pepper))
	return prefix, hash, usedAlgo, err
}

func (s *Service) hashAPIKey(key, algo string) (string, error) {
	return hashAPIKeyWithAlgo(key, algo, s.pepper)
}

func hashAPIKeyWithAlgo(key, algo string, pepper []byte) (string, error) {
	switch algo {
	case "", HashAlgoSHA256:
		sum := sha256.Sum256([]byte(key))
		return hex.EncodeToString(sum[:]), nil
	case HashAlgoHMACSHA256:
		if len(pepper) == 0 {
			return "", errors.New("API key pepper not configured")
		}
		mac := hmac.New(sha256.New, pepper)
		_, _ = mac.Write([]byte(key))
		return hex.EncodeToString(mac.Sum(nil)), nil
	default:
		return "", fmt.Errorf("unknown API key hash algo: %s", algo)
	}
}
