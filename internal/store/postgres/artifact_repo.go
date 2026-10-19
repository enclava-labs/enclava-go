package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"enclava-go/internal/artifacts"
)

// ArtifactRepo is the concrete artifacts.MetadataStore over the artifacts
// table (migration 004): every statement is scoped by the composite
// (tenant_id, id) primary key, and lease/fence transitions are conditional
// updates so a reclaimed writer can never finalize.
type ArtifactRepo struct {
	db *DB
}

func NewArtifactRepo(db *DB) *ArtifactRepo {
	return &ArtifactRepo{db: db}
}

var _ artifacts.MetadataStore = (*ArtifactRepo)(nil)

func (r *ArtifactRepo) BeginInsert(ctx context.Context, row artifacts.ArtifactRow) error {
	_, err := r.db.SQL.ExecContext(ctx, `
		INSERT INTO artifacts (tenant_id, id, kind, version, sha256, byte_size,
		                       mime_type, state, storage_path,
		                       lease_owner, lease_expires_at, fence_token)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		row.TenantID, row.ID, row.Kind, row.Version, row.SHA256, row.ByteSize,
		row.MimeType, row.State, row.StoragePath,
		row.LeaseOwner, row.LeaseExpiresAt, row.FenceToken)
	if err != nil {
		return fmt.Errorf("insert artifact row: %w", err)
	}
	return nil
}

func (r *ArtifactRepo) GetArtifact(ctx context.Context, tenantID, id string) (artifacts.ArtifactRow, error) {
	var row artifacts.ArtifactRow
	var leaseOwner, tombstoneReason sql.NullString
	var leaseExpiresAt sql.NullTime
	err := r.db.SQL.QueryRowContext(ctx, `
		SELECT tenant_id::text, id, kind, version, sha256, byte_size, mime_type,
		       state, storage_path, lease_owner, lease_expires_at, fence_token,
		       tombstone_reason
		FROM artifacts WHERE tenant_id = $1 AND id = $2`,
		tenantID, id).Scan(
		&row.TenantID, &row.ID, &row.Kind, &row.Version, &row.SHA256,
		&row.ByteSize, &row.MimeType, &row.State, &row.StoragePath,
		&leaseOwner, &leaseExpiresAt, &row.FenceToken, &tombstoneReason)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return artifacts.ArtifactRow{}, artifacts.ErrNotFound
	}
	if err != nil {
		return artifacts.ArtifactRow{}, fmt.Errorf("get artifact: %w", err)
	}
	row.LeaseOwner = leaseOwner.String
	if leaseExpiresAt.Valid {
		row.LeaseExpiresAt = leaseExpiresAt.Time
	}
	row.TombstoneReason = tombstoneReason.String
	return row, nil
}

func (r *ArtifactRepo) FinalizeCommit(ctx context.Context, tenantID, id, leaseOwner string, fenceToken int64, sha256 string, byteSize int64) error {
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE artifacts
		SET state = 'final', sha256 = $5, byte_size = $6,
		    finalized_at = now(), lease_owner = NULL, lease_expires_at = NULL
		WHERE tenant_id = $1 AND id = $2 AND state = 'writing'
		  AND lease_owner = $3 AND fence_token = $4`,
		tenantID, id, leaseOwner, fenceToken, sha256, byteSize)
	if err != nil {
		if isInvalidUUID(err) {
			return artifacts.ErrLeaseLost
		}
		return fmt.Errorf("finalize artifact: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("finalize artifact rows: %w", err)
	}
	if n == 0 {
		return artifacts.ErrLeaseLost
	}
	return nil
}

func (r *ArtifactRepo) UpdateState(ctx context.Context, tenantID, id, state, tombstoneReason string) error {
	switch state {
	case artifacts.StateDeleting, artifacts.StateTombstoned:
	default:
		return fmt.Errorf("artifact state %q: transitions other than deleting/tombstoned go through Begin/FinalizeCommit", state)
	}
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE artifacts
		SET state = $3,
		    tombstone_reason = CASE WHEN state <> 'tombstoned' THEN $4
		                            ELSE tombstone_reason END,
		    deleted_at = CASE WHEN state <> 'tombstoned' AND $3 = 'tombstoned'
		                      THEN now() ELSE deleted_at END
		WHERE tenant_id = $1 AND id = $2`,
		tenantID, id, state, nullableReason(tombstoneReason))
	if err != nil {
		if isInvalidUUID(err) {
			return artifacts.ErrNotFound
		}
		return fmt.Errorf("update artifact state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update artifact rows: %w", err)
	}
	if n == 0 {
		return artifacts.ErrNotFound
	}
	return nil
}

func (r *ArtifactRepo) DeleteRow(ctx context.Context, tenantID, id string) error {
	res, err := r.db.SQL.ExecContext(ctx,
		`DELETE FROM artifacts WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		if isInvalidUUID(err) {
			return artifacts.ErrNotFound
		}
		return fmt.Errorf("delete artifact row: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete artifact rows: %w", err)
	}
	if n == 0 {
		return artifacts.ErrNotFound
	}
	return nil
}

func (r *ArtifactRepo) ListFinalPaths(ctx context.Context) ([]artifacts.ArtifactRow, error) {
	return r.listRows(ctx, `WHERE state IN ('final','deleting','tombstoned')`)
}

func (r *ArtifactRepo) ListExpiredWriting(ctx context.Context, now time.Time) ([]artifacts.ArtifactRow, error) {
	return r.listRows(ctx,
		`WHERE state = 'writing' AND lease_expires_at IS NOT NULL AND lease_expires_at < $1`, now)
}

func (r *ArtifactRepo) InvalidateWriting(ctx context.Context, tenantID, id, leaseOwner string, fenceToken int64, tombstoneReason string) (bool, error) {
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE artifacts
		SET state = 'tombstoned', tombstone_reason = $5, deleted_at = now(),
		    lease_owner = NULL, lease_expires_at = NULL
		WHERE tenant_id = $1 AND id = $2 AND state = 'writing'
		  AND lease_owner = $3 AND fence_token = $4`,
		tenantID, id, leaseOwner, fenceToken, tombstoneReason)
	if err != nil {
		if isInvalidUUID(err) {
			return false, nil
		}
		return false, fmt.Errorf("invalidate writing artifact: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("invalidate writing rows: %w", err)
	}
	return n == 1, nil
}

func (r *ArtifactRepo) listRows(ctx context.Context, where string, args ...any) ([]artifacts.ArtifactRow, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT tenant_id::text, id, kind, version, sha256, byte_size, mime_type,
		       state, storage_path, lease_owner, lease_expires_at, fence_token,
		       tombstone_reason
		FROM artifacts `+where+` ORDER BY tenant_id, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	defer rows.Close()
	out := []artifacts.ArtifactRow{}
	for rows.Next() {
		var row artifacts.ArtifactRow
		var leaseOwner, tombstoneReason sql.NullString
		var leaseExpiresAt sql.NullTime
		if err := rows.Scan(&row.TenantID, &row.ID, &row.Kind, &row.Version,
			&row.SHA256, &row.ByteSize, &row.MimeType, &row.State,
			&row.StoragePath, &leaseOwner, &leaseExpiresAt, &row.FenceToken,
			&tombstoneReason); err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		row.LeaseOwner = leaseOwner.String
		if leaseExpiresAt.Valid {
			row.LeaseExpiresAt = leaseExpiresAt.Time
		}
		row.TombstoneReason = tombstoneReason.String
		out = append(out, row)
	}
	return out, rows.Err()
}

func nullableReason(reason string) any {
	if reason == "" {
		return nil
	}
	return reason
}

// isInvalidUUID treats a malformed tenant_id as "no such row" — an unknown
// identifier gets the same outcome as a forbidden one.
func isInvalidUUID(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SQLSTATE 22P02")
}
