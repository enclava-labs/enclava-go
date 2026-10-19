package auth

import (
	"context"
	"encoding/json"
	"time"
)

// ScopePlatformAdmin is the scope granting platform administration.
const ScopePlatformAdmin = "platform.admin"

// User is a tenant-scoped local account. PasswordHash is the bcrypt hash
// stored on the row; plaintext passwords never appear on any record.
type User struct {
	ID            string
	TenantID      string
	Email         string
	DisplayName   string
	Status        string // "active" | "disabled"
	PasswordHash  string
	PasswordSetAt *time.Time
	LastLoginAt   *time.Time
	CreatedAt     time.Time
}

// Role is a named scope bundle assigned to users.
type Role struct {
	ID        string
	Name      string
	Scopes    []string
	CreatedAt time.Time
}

// ServiceAccount is a tenant-scoped non-human actor with its own scope set.
type ServiceAccount struct {
	ID        string
	TenantID  string
	Name      string
	Scopes    []string
	Status    string // "active" | "disabled"
	CreatedAt time.Time
}

// Session is a server-revocable authenticated session record. TokenHash is
// sha256 of the opaque bearer token; CSRFToken is issued alongside it and
// required for cookie-authenticated mutations.
type Session struct {
	ID            string
	TenantID      string
	UserID        string
	TokenHash     string
	CSRFToken     string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	IdleExpiresAt time.Time
	RevokedAt     *time.Time
	RevokeReason  string
}

// Delegation records that a delegator user authorized a delegate service
// account to exercise a subset of authority. AllowedScopes is the stored
// allowlist JSONB ({"mode":"list","items":[...]}).
type Delegation struct {
	ID                       string
	TenantID                 string
	DelegatorUserID          string
	DelegateServiceAccountID string
	AllowedScopes            json.RawMessage
	AllowedCollections       json.RawMessage
	ExpiresAt                time.Time
	RevokedAt                *time.Time
	CreatedAt                time.Time
}

// AuthIdentity links an external issuer+subject to a user.
type AuthIdentity struct {
	ID             string
	UserID         string
	Issuer         string
	Subject        string
	LinkedAt       time.Time
	LinkedByUserID string
}

// OIDCProvider is a tenant-configured identity provider row.
type OIDCProvider struct {
	ID              string
	Name            string
	Issuer          string
	ClientID        string
	ClientSecretRef string
	RedirectPath    string
	Enabled         bool
	CreatedAt       time.Time
}

// IdentityRepository owns every identity row consumed by authentication
// (migration 002): tenants, users, roles, service accounts, API keys,
// sessions, auth identities, delegations and OIDC providers.
//
// Every method is tenant-scoped EXCEPT the two pre-authentication locators,
// which are the narrowly scoped global exception — a credential lookup must
// find the row before its tenant is known: FindKeyCandidates (bounded
// key-prefix candidate scan) and GetSessionByTokenHash (unique token-hash
// lookup). The service verifies the secret/hash, expiry and revocation
// BEFORE trusting the row's tenant_id; everything after is tenant-scoped.
type IdentityRepository interface {
	// Tenants (bootstrap/admin writes).
	CreateTenant(ctx context.Context, slug, displayName string) (tenantID string, err error)

	// Roles.
	CreateRole(ctx context.Context, tenantID string, r Role) error
	AssignRole(ctx context.Context, tenantID, userID, roleID string) error
	RolesForUser(ctx context.Context, tenantID, userID string) ([]Role, error)

	// Users.
	CreateUser(ctx context.Context, tenantID string, u User) error
	GetUser(ctx context.Context, tenantID, userID string) (User, error)
	GetUserByEmail(ctx context.Context, tenantID, email string) (User, error)
	UpdateUserStatus(ctx context.Context, tenantID, userID, status string) error
	SetPassword(ctx context.Context, tenantID, userID, hash string, at time.Time) error
	TouchLastLogin(ctx context.Context, tenantID, userID string, at time.Time) error

	// Service accounts.
	CreateServiceAccount(ctx context.Context, tenantID string, sa ServiceAccount) error
	GetServiceAccount(ctx context.Context, tenantID, id string) (ServiceAccount, error)
	UpdateServiceAccountStatus(ctx context.Context, tenantID, id, status string) error

	// API keys.
	CreateAPIKey(ctx context.Context, tenantID string, k APIKeyRecord) error
	GetAPIKey(ctx context.Context, tenantID, id string) (APIKeyRecord, error)
	ListAPIKeys(ctx context.Context, tenantID string) ([]APIKeyRecord, error)
	UpdateAPIKey(ctx context.Context, tenantID string, k APIKeyRecord) error
	RevokeAPIKey(ctx context.Context, tenantID, id string, at time.Time) error
	TouchAPIKeyUsage(ctx context.Context, tenantID, id string, at time.Time) error
	// FindKeyCandidates is the pre-authentication locator: verify the secret,
	// expiry and revocation before trusting the row's tenant.
	FindKeyCandidates(ctx context.Context, prefix string) ([]APIKeyRecord, error)

	// Sessions.
	CreateSession(ctx context.Context, tenantID string, s Session) error
	GetSession(ctx context.Context, tenantID, id string) (Session, error)
	// GetSessionByTokenHash is the pre-authentication locator: verify expiry,
	// idle and revocation before trusting the row's tenant.
	GetSessionByTokenHash(ctx context.Context, tokenHash string) (Session, error)
	TouchSession(ctx context.Context, tenantID, id string, idleUntil time.Time) error
	RevokeSession(ctx context.Context, tenantID, id string, at time.Time, reason string) error
	RevokeUserSessions(ctx context.Context, tenantID, userID string, at time.Time, reason string) error

	// External identities.
	RecordAuthIdentity(ctx context.Context, tenantID string, a AuthIdentity) error
	GetAuthIdentity(ctx context.Context, tenantID, issuer, subject string) (AuthIdentity, error)

	// Delegations.
	CreateDelegation(ctx context.Context, tenantID string, d Delegation) error
	GetDelegation(ctx context.Context, tenantID, id string) (Delegation, error)
	FindDelegation(ctx context.Context, tenantID, delegatorUserID, delegateServiceAccountID string) (Delegation, error)

	// OIDC providers (read side + admin write).
	GetOIDCProvider(ctx context.Context, tenantID, id string) (OIDCProvider, error)
	ListOIDCProviders(ctx context.Context, tenantID string) ([]OIDCProvider, error)
	CreateOIDCProvider(ctx context.Context, tenantID string, p OIDCProvider) error
}
