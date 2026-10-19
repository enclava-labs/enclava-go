package auth

import (
	"context"
	"time"
)

// Session is a server-revocable authenticated session record.
type Session struct {
	ID        string
	TenantID  string
	ActorID   string
	UserID    string
	TokenHash string
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// Identity is a tenant-scoped user or service-account actor.
type Identity struct {
	ID        string
	TenantID  string
	ActorID   string
	Kind      ActorKind
	CreatedAt time.Time
	RevokedAt *time.Time
}

// Delegation records that DelegatedByActorID authorized ActorID's authority
// to be exercised under a derived principal.
type Delegation struct {
	ID                 string
	TenantID           string
	ActorID            string
	DelegatedByActorID string
	CreatedAt          time.Time
	RevokedAt          *time.Time
}

// Tenant-scoped repository seams. APIKeyRepository is intentionally not
// redeclared here — it stays in service.go until 01-08 moves and reshapes it.
// The pre-auth session token-hash lookup is the narrow documented exception
// to tenant scoping; all other reads are tenant-scoped.
type SessionRepository interface {
	// FindByTokenHash is the pre-authentication exception: verify the stored
	// actor, expiry and revocation before accepting the row's tenant.
	FindByTokenHash(ctx context.Context, tokenHash string) (Session, error)
	Get(ctx context.Context, tenantID, id string) (Session, error)
	Create(ctx context.Context, s Session) error
	Revoke(ctx context.Context, tenantID, id string, at time.Time) error
}

type IdentityRepository interface {
	Get(ctx context.Context, tenantID, actorID string) (Identity, error)
	List(ctx context.Context, tenantID string) ([]Identity, error)
	Create(ctx context.Context, identity Identity) error
	Revoke(ctx context.Context, tenantID, actorID string, at time.Time) error
}

type DelegationRepository interface {
	Get(ctx context.Context, tenantID, id string) (Delegation, error)
	ListForActor(ctx context.Context, tenantID, actorID string) ([]Delegation, error)
	Create(ctx context.Context, d Delegation) error
	Revoke(ctx context.Context, tenantID, id string, at time.Time) error
}
