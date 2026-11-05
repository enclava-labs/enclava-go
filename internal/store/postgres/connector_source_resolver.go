package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"enclava-go/internal/source"
)

func (r *SourceRepo) ObjectForNativeID(ctx context.Context, tenantID, connectionID, kind, nativeID string) (source.Object, error) {
	var id string
	err := r.db.SQL.QueryRowContext(ctx, `SELECT id FROM source_objects WHERE tenant_id=$1 AND connection_id=$2 AND kind=$3 AND native_id=$4`, tenantID, connectionID, kind, nativeID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return source.Object{}, source.ErrDenied
	}
	if err != nil {
		return source.Object{}, fmt.Errorf("resolve source object: %w", err)
	}
	return r.GetObject(ctx, tenantID, id)
}

func (r *SourceRepo) ObjectForNativeIDCrossKind(ctx context.Context, tenantID, connectionID, nativeID string) (source.Object, error) {
	var id string
	err := r.db.SQL.QueryRowContext(ctx, `SELECT id FROM source_objects WHERE tenant_id=$1 AND connection_id=$2 AND native_id=$3 ORDER BY observed_seq DESC LIMIT 1`, tenantID, connectionID, nativeID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return source.Object{}, source.ErrDenied
	}
	if err != nil {
		return source.Object{}, fmt.Errorf("resolve source object cross-kind: %w", err)
	}
	return r.GetObject(ctx, tenantID, id)
}
