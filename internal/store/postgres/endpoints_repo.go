// endpoints_repo.go implements inference.EndpointRepository over migration
// 007 — tenant-scoped inference endpoint records binding protocol,
// capabilities, base origin, credential reference and real assurance state
// (INF-01). base_origin is validated as an absolute https origin; http is
// accepted only for explicitly authorized private endpoints.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"enclava-go/internal/inference"
)

// EndpointsRepo is the postgres-backed inference.EndpointRepository.
type EndpointsRepo struct {
	db *sql.DB
}

// NewEndpointsRepo wraps the shared pool.
func NewEndpointsRepo(db *DB) *EndpointsRepo {
	return &EndpointsRepo{db: db.SQL}
}

var _ inference.EndpointRepository = (*EndpointsRepo)(nil)

const endpointCols = `id, protocol, base_origin, credential_ref,
	capabilities::text, assurance_mode, assurance_verified_at,
	authorized_private, enabled`

func scanEndpoint(row rowScanner, tenantID string) (inference.Endpoint, error) {
	var e inference.Endpoint
	var caps string
	var verified sql.NullTime
	err := row.Scan(&e.ID, &e.Protocol, &e.BaseOrigin, &e.CredentialRef,
		&caps, &e.AssuranceMode, &verified, &e.AuthorizedPrivate, &e.Enabled)
	if err != nil {
		return inference.Endpoint{}, err
	}
	if err := json.Unmarshal([]byte(caps), &e.Capabilities); err != nil {
		return inference.Endpoint{}, fmt.Errorf("decode capabilities: %w", err)
	}
	if verified.Valid {
		tv := verified.Time
		e.AssuranceVerifiedAt = &tv
	}
	e.TenantID = tenantID
	return e, nil
}

// validateEndpointOrigin enforces the origin contract: absolute https for
// public endpoints; explicit http only when the administrator authorized a
// private endpoint. Path, query, fragment and userinfo are not origins.
func validateEndpointOrigin(e inference.Endpoint) error {
	u, err := url.Parse(e.BaseOrigin)
	if err != nil || u.Host == "" {
		return fmt.Errorf("inference: base_origin %q is not an absolute origin", e.BaseOrigin)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "https":
	case "http":
		if !e.AuthorizedPrivate {
			return fmt.Errorf("inference: http base_origin %q requires authorized_private", e.BaseOrigin)
		}
	default:
		return fmt.Errorf("inference: base_origin scheme %q rejected", u.Scheme)
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("inference: base_origin %q must be a bare origin (no path/query/fragment/userinfo)", e.BaseOrigin)
	}
	if e.Protocol == "" {
		return fmt.Errorf("inference: protocol is required")
	}
	return nil
}

// Get returns one tenant-scoped endpoint; unknown or foreign ids are absent.
func (r *EndpointsRepo) Get(ctx context.Context, tenantID, id string) (inference.Endpoint, error) {
	e, err := scanEndpoint(r.db.QueryRowContext(ctx, `
		SELECT `+endpointCols+`
		FROM inference_endpoints WHERE tenant_id = $1::uuid AND id = $2`,
		tenantID, id), tenantID)
	if err != nil {
		if err == sql.ErrNoRows || isInvalidUUID(err) {
			return inference.Endpoint{}, fmt.Errorf("%w: %s", inference.ErrEndpointNotFound, id)
		}
		return inference.Endpoint{}, fmt.Errorf("get endpoint: %w", err)
	}
	return e, nil
}

// List returns every endpoint of the tenant, ordered for determinism.
func (r *EndpointsRepo) List(ctx context.Context, tenantID string) ([]inference.Endpoint, error) {
	return r.listWhere(ctx, tenantID, "", nil)
}

// ListCapabilities returns the tenant's enabled endpoints advertising the
// given capability — the routing input the executor consumes.
func (r *EndpointsRepo) ListCapabilities(ctx context.Context, tenantID, capability string) ([]inference.Endpoint, error) {
	return r.listWhere(ctx, tenantID,
		"AND enabled AND capabilities @> $2::jsonb", []any{mustJSON(capability)})
}

func (r *EndpointsRepo) listWhere(ctx context.Context, tenantID, extra string, args []any) ([]inference.Endpoint, error) {
	q := `SELECT ` + endpointCols + ` FROM inference_endpoints WHERE tenant_id = $1::uuid ` + extra + ` ORDER BY id`
	rows, err := r.db.QueryContext(ctx, q, append([]any{tenantID}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("list endpoints: %w", err)
	}
	defer rows.Close()
	var out []inference.Endpoint
	for rows.Next() {
		e, err := scanEndpoint(rows, tenantID)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Create stores a new tenant-scoped endpoint record.
func (r *EndpointsRepo) Create(ctx context.Context, e inference.Endpoint) error {
	if err := validateEndpointOrigin(e); err != nil {
		return err
	}
	caps, err := json.Marshal(e.Capabilities)
	if err != nil {
		return fmt.Errorf("marshal capabilities: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO inference_endpoints
			(tenant_id, id, protocol, base_origin, credential_ref, capabilities,
			 assurance_mode, assurance_verified_at, authorized_private, enabled)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		e.TenantID, e.ID, e.Protocol, e.BaseOrigin, e.CredentialRef,
		string(caps), e.AssuranceMode, e.AssuranceVerifiedAt,
		e.AuthorizedPrivate, e.Enabled)
	if err != nil {
		return fmt.Errorf("create endpoint: %w", err)
	}
	return nil
}

// Update rewrites a tenant-scoped endpoint record; assurance fields persist
// independently of authorized_private.
func (r *EndpointsRepo) Update(ctx context.Context, e inference.Endpoint) error {
	if err := validateEndpointOrigin(e); err != nil {
		return err
	}
	caps, err := json.Marshal(e.Capabilities)
	if err != nil {
		return fmt.Errorf("marshal capabilities: %w", err)
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE inference_endpoints SET
			protocol = $3, base_origin = $4, credential_ref = $5,
			capabilities = $6, assurance_mode = $7, assurance_verified_at = $8,
			authorized_private = $9, enabled = $10, updated_at = now()
		WHERE tenant_id = $1::uuid AND id = $2`,
		e.TenantID, e.ID, e.Protocol, e.BaseOrigin, e.CredentialRef,
		string(caps), e.AssuranceMode, e.AssuranceVerifiedAt,
		e.AuthorizedPrivate, e.Enabled)
	if err != nil {
		return fmt.Errorf("update endpoint: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", inference.ErrEndpointNotFound, e.ID)
	}
	return nil
}

// SetEnabled toggles availability without touching configuration.
func (r *EndpointsRepo) SetEnabled(ctx context.Context, tenantID, id string, enabled bool) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE inference_endpoints SET enabled = $3, updated_at = now()
		WHERE tenant_id = $1::uuid AND id = $2`, tenantID, id, enabled)
	if err != nil {
		return fmt.Errorf("set endpoint enabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", inference.ErrEndpointNotFound, id)
	}
	return nil
}

// mustJSON encodes one string as a single-element JSON array for @> queries.
func mustJSON(capability string) string {
	raw, err := json.Marshal([]string{capability})
	if err != nil {
		panic(err)
	}
	return string(raw)
}
