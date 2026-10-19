// Package inference holds the canonical inference endpoint record and the
// tenant-scoped repository seam consumed by the transports and executor.
// Pure contract types — no store, llm or rag imports.
package inference

import (
	"context"
	"time"
)

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
}
