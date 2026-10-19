// Package inference holds the canonical inference endpoint record and the
// tenant-scoped repository seam consumed by the transports and executor.
// Pure contract types — no store, llm or rag imports.
package inference

import (
	"context"
	"errors"
	"time"
)

// ErrEndpointNotFound is returned for unknown or cross-tenant endpoint ids —
// the same outcome either way.
var ErrEndpointNotFound = errors.New("inference: endpoint not found")

// Endpoint is a tenant-scoped configured inference endpoint. Administrative
// configuration must explicitly authorize a private endpoint
// (AuthorizedPrivate); AssuranceVerifiedAt records real attested verification
// — local is never inferred confidential and ordinary TLS is not an enclave.
type Endpoint struct {
	ID                  string
	TenantID            string
	Protocol            string
	Capabilities        []string
	BaseOrigin          string
	CredentialRef       string // server-approved, bound to tenant+purpose+origin
	AssuranceMode       string
	AssuranceVerifiedAt *time.Time
	AuthorizedPrivate   bool
	Enabled             bool
}

// EndpointRepository is the tenant-scoped persistence seam for Endpoint
// records, consumed by the transports and executor.
type EndpointRepository interface {
	Get(ctx context.Context, tenantID, id string) (Endpoint, error)
	List(ctx context.Context, tenantID string) ([]Endpoint, error)
	Create(ctx context.Context, e Endpoint) error
	Update(ctx context.Context, e Endpoint) error
	SetEnabled(ctx context.Context, tenantID, id string, enabled bool) error
	// ListCapabilities returns the tenant's enabled endpoints advertising the
	// capability — deterministic routing input for the executor.
	ListCapabilities(ctx context.Context, tenantID, capability string) ([]Endpoint, error)
}
