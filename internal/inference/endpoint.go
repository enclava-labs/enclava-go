// Package inference holds the canonical inference endpoint record and the
// tenant-scoped repository seam consumed by the transports and executor.
// Pure contract types — no store, llm or rag imports.
package inference

import (
	"context"
	"errors"
	"net/url"
	"strings"
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
	ID            string
	TenantID      string
	Protocol      string
	SDKProvider   string
	Capabilities  []string
	BaseOrigin    string
	CredentialRef string
	// EnclaveRepo is the attested repository reference a dedicated tinfoil
	// enclave verifies against (owner/name[@tag][@sha256:digest]). It is set
	// only on tinfoil endpoints whose origin is a dedicated enclave; the
	// shared router host uses the SDK's router configuration instead.
	EnclaveRepo         string
	AssuranceMode       string
	AssuranceVerifiedAt *time.Time
	AuthorizedPrivate   bool
	Enabled             bool
	// RequestDefaults are operator-set top-level body fields (for example
	// temperature or chat_template_kwargs) added to chat requests that do not
	// set them; stored in the endpoint's config and exposed there by the API.
	RequestDefaults map[string]any `json:"-"`
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
	ListCapabilities(ctx context.Context, tenantID string, capability string) ([]Endpoint, error)
}

// DefaultTinfoilEnclave is the host the tinfoil SDK treats as its router:
// a client built with no explicit enclave discovers and verifies the router
// automatically, while any other host is a dedicated enclave that needs an
// explicit repo to verify against.
const DefaultTinfoilEnclave = "inference.tinfoil.sh"

// TinfoilEnclaveHost returns the enclave host an endpoint's base origin names
// — bare hostname for default ports, host:port otherwise. A non-origin or
// empty origin reports "".
func TinfoilEnclaveHost(origin string) string {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || u.Host == "" {
		return ""
	}
	if strings.EqualFold(u.Scheme, "https") && u.Port() == "443" {
		return strings.ToLower(u.Hostname())
	}
	return strings.ToLower(u.Host)
}
