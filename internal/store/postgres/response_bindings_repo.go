package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Retained Responses references are bound rows: a public opaque ID is usable
// only where its binding says it is — same tenant, same endpoint origin,
// owning actor (or its recorded delegator), live state. A raw upstream ID
// held by a caller is never authority; identical upstream IDs on different
// endpoints (or under different parent responses) bind to distinct rows.

// ResponseBindingKind selects the retained resource family a binding covers.
const (
	ResponseBindingResponse = "response"
	ResponseBindingItem     = "item"
)

// ErrResponseBindingNotFound is the single outcome for unknown, cross-tenant
// or tombstoned references — resolution never distinguishes them.
var ErrResponseBindingNotFound = errors.New("postgres: response binding not found")

// ErrPublicIDConflict means the minted public ID already exists; callers
// retry Bind with a fresh ID rather than returning it to any client.
var ErrPublicIDConflict = errors.New("postgres: response binding public id conflict")

// ResponseBinding is one durable public↔upstream reference row.
type ResponseBinding struct {
	TenantID       string
	PublicID       string
	Kind           string // ResponseBinding* const set
	EndpointID     string
	OwnerActorID   string
	DelegatedBy    string
	UpstreamID     string
	ParentPublicID string
	// InputSnapshot is the immutable input/restriction record the reference
	// was created under: {"inputs":[...],"artifacts":[...],
	// "conversations":[...],"restriction":{...}} — caller-supplied JSON.
	InputSnapshot json.RawMessage
	State         string // "active" | "deleted"
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ResponseBindingsRepo persists response/item owner+origin bindings.
type ResponseBindingsRepo struct {
	db *DB
}

func NewResponseBindingsRepo(db *DB) *ResponseBindingsRepo {
	return &ResponseBindingsRepo{db: db}
}

// Bind persists one public↔upstream reference. Idempotent for an identical
// binding (same tenant/endpoint/parent/upstream re-insert returns the
// existing row); a conflicting reused public ID reports ErrPublicIDConflict
// so the caller mints again. The row exists before any public ID is exposed —
// a Bind error means no usable reference was created.
func (r *ResponseBindingsRepo) Bind(ctx context.Context, b ResponseBinding) (ResponseBinding, error) {
	if b.TenantID == "" || b.PublicID == "" || b.EndpointID == "" ||
		b.OwnerActorID == "" || b.UpstreamID == "" {
		return ResponseBinding{}, fmt.Errorf("postgres: response binding missing required fields")
	}
	if b.Kind != ResponseBindingResponse && b.Kind != ResponseBindingItem {
		return ResponseBinding{}, fmt.Errorf("postgres: response binding kind %q", b.Kind)
	}
	if b.Kind == ResponseBindingItem && b.ParentPublicID == "" {
		return ResponseBinding{}, fmt.Errorf("postgres: item binding requires parent response")
	}
	snapshot := b.InputSnapshot
	if len(snapshot) == 0 {
		snapshot = json.RawMessage(`{}`)
	}
	row, err := scanResponseBinding(r.db.SQL.QueryRowContext(ctx, `
		INSERT INTO response_bindings
		    (tenant_id, public_id, kind, endpoint_id, owner_actor_id,
		     delegated_by, upstream_id, parent_public_id, input_snapshot)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (tenant_id, endpoint_id, parent_public_id, upstream_id)
		DO NOTHING
		RETURNING tenant_id, public_id, kind, endpoint_id, owner_actor_id,
		    delegated_by, upstream_id, parent_public_id, input_snapshot,
		    state, created_at, updated_at`,
		b.TenantID, b.PublicID, b.Kind, b.EndpointID, b.OwnerActorID,
		b.DelegatedBy, b.UpstreamID, b.ParentPublicID, snapshot))
	if err == nil {
		return row, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		// PK conflict: the minted public id is taken by a different binding.
		return ResponseBinding{}, ErrPublicIDConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ResponseBinding{}, fmt.Errorf("bind response reference: %w", err)
	}
	// Conflict target hit — the same upstream reference is already bound.
	existing, gerr := r.getByUpstream(ctx, b.TenantID, b.EndpointID, b.ParentPublicID, b.UpstreamID)
	if gerr != nil {
		return ResponseBinding{}, fmt.Errorf("bind response reference: %w", err)
	}
	return existing, nil
}

// Resolve returns the active binding behind a public ID. Wrong tenant,
// unknown or tombstoned references all report ErrResponseBindingNotFound.
func (r *ResponseBindingsRepo) Resolve(ctx context.Context, tenantID, publicID string) (ResponseBinding, error) {
	row, err := scanResponseBinding(r.db.SQL.QueryRowContext(ctx, `
		SELECT tenant_id, public_id, kind, endpoint_id, owner_actor_id,
		    delegated_by, upstream_id, parent_public_id, input_snapshot,
		    state, created_at, updated_at
		FROM response_bindings
		WHERE tenant_id = $1 AND public_id = $2`,
		tenantID, publicID))
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return ResponseBinding{}, ErrResponseBindingNotFound
	}
	if err != nil {
		return ResponseBinding{}, fmt.Errorf("resolve response binding: %w", err)
	}
	if row.State != "active" {
		return ResponseBinding{}, ErrResponseBindingNotFound
	}
	return row, nil
}

// GetByUpstream returns the binding for a raw upstream reference — used to
// re-map already-bound IDs inside upstream payloads. Tombstoned rows still
// resolve so later payloads referencing them keep their public identity.
func (r *ResponseBindingsRepo) GetByUpstream(ctx context.Context, tenantID, endpointID, parentPublicID, upstreamID string) (ResponseBinding, error) {
	return r.getByUpstream(ctx, tenantID, endpointID, parentPublicID, upstreamID)
}

func (r *ResponseBindingsRepo) getByUpstream(ctx context.Context, tenantID, endpointID, parentPublicID, upstreamID string) (ResponseBinding, error) {
	row, err := scanResponseBinding(r.db.SQL.QueryRowContext(ctx, `
		SELECT tenant_id, public_id, kind, endpoint_id, owner_actor_id,
		    delegated_by, upstream_id, parent_public_id, input_snapshot,
		    state, created_at, updated_at
		FROM response_bindings
		WHERE tenant_id = $1 AND endpoint_id = $2
		  AND parent_public_id = $3 AND upstream_id = $4`,
		tenantID, endpointID, parentPublicID, upstreamID))
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return ResponseBinding{}, ErrResponseBindingNotFound
	}
	if err != nil {
		return ResponseBinding{}, fmt.Errorf("get response binding: %w", err)
	}
	return row, nil
}

// MarkDeleted tombstones a binding by public ID after its upstream resource
// was deleted. Unknown/foreign rows report not-found — never a silent no-op
// presented as success.
func (r *ResponseBindingsRepo) MarkDeleted(ctx context.Context, tenantID, publicID string) error {
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE response_bindings
		SET state = 'deleted', updated_at = now()
		WHERE tenant_id = $1 AND public_id = $2 AND state = 'active'`,
		tenantID, publicID)
	if err != nil {
		return fmt.Errorf("tombstone response binding: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrResponseBindingNotFound
	}
	return nil
}

func scanResponseBinding(row *sql.Row) (ResponseBinding, error) {
	var b ResponseBinding
	err := row.Scan(&b.TenantID, &b.PublicID, &b.Kind, &b.EndpointID,
		&b.OwnerActorID, &b.DelegatedBy, &b.UpstreamID, &b.ParentPublicID,
		&b.InputSnapshot, &b.State, &b.CreatedAt, &b.UpdatedAt)
	return b, err
}
