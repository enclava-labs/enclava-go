// api_access_repo.go — the migration-048 api_access_requests store plus the
// member-facing own-key usage projection the Account developer card reads.
// api_access_requests lives on IdentityRepo (the identity store) so the
// dashboard needs no extra wiring: requests are tenant-scoped, at most one
// open per user is enforced by the partial unique index, and dismissing is
// a status stamp — rows are kept as the request/audit trail.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"enclava-go/internal/auth"
)

// APIAccessRequest is one stored member request for an API key.
type APIAccessRequest struct {
	ID          string
	UserID      string
	UserName    string // requester's display name/email at list time
	Note        string
	Status      string // "open" | "dismissed"
	CreatedAt   time.Time
	DismissedAt *time.Time
	DismissedBy string
}

// APIKeyUsage is one caller-owned API key with its settlement-derived token
// totals — display data for the developer card; cost stays out of it.
type APIKeyUsage struct {
	ID           string
	Name         string
	KeyPrefix    string
	LastUsedAt   *time.Time
	InputTokens  int64
	CachedTokens int64
	OutputTokens int64
	TotalTokens  int64
}

// RequestAPIAccess records the caller's request. created is false when the
// user already holds an open request — the row count never exceeds one open
// per user (the partial unique index is the authority; the check is the
// friendly path).
func (r *IdentityRepo) RequestAPIAccess(ctx context.Context, tenantID, id, userID, note string, at time.Time) (created bool, err error) {
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO api_access_requests (tenant_id, id, user_id, note, created_at)
		 VALUES ($1::uuid, $2, $3, $4, $5)
		 ON CONFLICT (tenant_id, user_id) WHERE status = 'open' DO NOTHING`,
		tenantID, id, userID, note, at)
	if err != nil {
		return false, fmt.Errorf("request api access: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("request api access: %w", err)
	}
	return n > 0, nil
}

// OpenAPIAccessRequest returns the user's open request for the card's
// pending state; ErrNotFound when none is open.
func (r *IdentityRepo) OpenAPIAccessRequest(ctx context.Context, tenantID, userID string) (APIAccessRequest, error) {
	var req APIAccessRequest
	err := r.db.QueryRowContext(ctx,
		`SELECT id, note, created_at
		 FROM api_access_requests
		 WHERE tenant_id = $1::uuid AND user_id = $2 AND status = 'open'`,
		tenantID, userID).
		Scan(&req.ID, &req.Note, &req.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			return APIAccessRequest{}, auth.ErrNotFound
		}
		return APIAccessRequest{}, fmt.Errorf("open api access request: %w", err)
	}
	req.UserID = userID
	req.Status = "open"
	return req, nil
}

// ListOpenAPIAccessRequests lists every open request for the tenant, oldest
// first, with the requester's name resolved at read time. Admin-only callers.
func (r *IdentityRepo) ListOpenAPIAccessRequests(ctx context.Context, tenantID string) ([]APIAccessRequest, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT r.id, r.user_id,
		        COALESCE(NULLIF(u.display_name, ''), u.email),
		        r.note, r.created_at
		 FROM api_access_requests r
		 JOIN users u ON u.tenant_id = r.tenant_id AND u.id = r.user_id
		 WHERE r.tenant_id = $1::uuid AND r.status = 'open'
		 ORDER BY r.created_at, r.id`,
		tenantID)
	if err != nil {
		return nil, fmt.Errorf("list api access requests: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []APIAccessRequest{}
	for rows.Next() {
		var req APIAccessRequest
		if err := rows.Scan(&req.ID, &req.UserID, &req.UserName, &req.Note, &req.CreatedAt); err != nil {
			return nil, fmt.Errorf("list api access requests: scan: %w", err)
		}
		req.Status = "open"
		out = append(out, req)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list api access requests: %w", err)
	}
	return out, nil
}

// DismissAPIAccessRequest stamps an open request dismissed. Unknown or
// already-closed ids share ErrNotFound — the admin surface never reveals
// whether a request existed in another state.
func (r *IdentityRepo) DismissAPIAccessRequest(ctx context.Context, tenantID, id, actorID string, at time.Time) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE api_access_requests
		 SET status = 'dismissed', dismissed_at = $3, dismissed_by = $4
		 WHERE tenant_id = $1::uuid AND id = $2 AND status = 'open'`,
		tenantID, id, at, actorID)
	if err != nil {
		return fmt.Errorf("dismiss api access request: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("dismiss api access request: %w", err)
	}
	if n == 0 {
		return auth.ErrNotFound
	}
	return nil
}

// ListAPIKeyUsageForOwner projects the caller's live keys and settled tokens.
func (r *IdentityRepo) ListAPIKeyUsageForOwner(ctx context.Context, tenantID, ownerUserID string) ([]APIKeyUsage, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT k.id, k.name, k.key_prefix, k.last_used_at,
		        COALESCE(u.input_tokens, 0), COALESCE(u.cached_tokens, 0),
		        COALESCE(u.output_tokens, 0), COALESCE(u.total_tokens, 0)
		 FROM api_keys k
		 LEFT JOIN LATERAL (
		     SELECT
		       COALESCE(sum((r.units->>'input')::bigint), 0) AS input_tokens,
		       COALESCE(sum((r.units->>'cached_input')::bigint), 0) AS cached_tokens,
		       COALESCE(sum((r.units->>'output')::bigint), 0) AS output_tokens,
		       COALESCE(sum(COALESCE((r.units->>'input')::bigint, 0) +
		                    COALESCE((r.units->>'cached_input')::bigint, 0) +
		                    COALESCE((r.units->>'output')::bigint, 0)), 0) AS total_tokens
		     FROM usage_records r
		     WHERE r.tenant_id = k.tenant_id
		       AND (r.credential_id = k.id OR (r.subject_type = 'api_key' AND r.subject_id = k.id))
		 ) AS u ON true
		 WHERE k.tenant_id = $1::uuid AND k.owner_user_id = $2 AND k.revoked_at IS NULL
		 ORDER BY k.created_at, k.id`,
		tenantID, ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("list own api key usage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []APIKeyUsage{}
	for rows.Next() {
		var k APIKeyUsage
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.LastUsedAt,
			&k.InputTokens, &k.CachedTokens, &k.OutputTokens, &k.TotalTokens); err != nil {
			return nil, fmt.Errorf("list own api key usage: scan: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list own api key usage: %w", err)
	}
	return out, nil
}
