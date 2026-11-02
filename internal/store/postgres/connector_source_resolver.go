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

func (r *SourceRepo) SourceIdentityForNativeID(ctx context.Context, tenantID, connectionID, nativeID string) (source.SourceIdentity, error) {
	var out source.SourceIdentity
	var mapped sql.NullString
	err := r.db.SQL.QueryRowContext(ctx, `SELECT tenant_id::text,id,connection_id,source_user_native_id,mapped_user_id FROM source_identities WHERE tenant_id=$1 AND connection_id=$2 AND source_user_native_id=$3`, tenantID, connectionID, nativeID).Scan(&out.TenantID, &out.ID, &out.ConnectionID, &out.NativeID, &mapped)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return source.SourceIdentity{}, source.ErrDenied
	}
	if err != nil {
		return source.SourceIdentity{}, fmt.Errorf("resolve source identity: %w", err)
	}
	if !mapped.Valid {
		return source.SourceIdentity{}, source.ErrDenied
	}
	out.MappedUserID = mapped.String
	return out, nil
}

func (r *SourceRepo) ObservedSourceIdentity(ctx context.Context, tenantID, connectionID, nativeID string) (source.SourceIdentity, error) {
	var out source.SourceIdentity
	var mapped sql.NullString
	err := r.db.SQL.QueryRowContext(ctx, `SELECT tenant_id::text,id,connection_id,source_user_native_id,mapped_user_id FROM source_identities WHERE tenant_id=$1 AND connection_id=$2 AND source_user_native_id=$3`, tenantID, connectionID, nativeID).Scan(&out.TenantID, &out.ID, &out.ConnectionID, &out.NativeID, &mapped)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return source.SourceIdentity{}, source.ErrDenied
	}
	if err != nil {
		return source.SourceIdentity{}, err
	}
	out.MappedUserID = mapped.String
	return out, nil
}

func (r *SourceRepo) SourceGroupForNativeID(ctx context.Context, tenantID, connectionID, nativeID string) (source.SourceGroup, error) {
	var out source.SourceGroup
	err := r.db.SQL.QueryRowContext(ctx, `SELECT tenant_id::text,id,connection_id,source_group_native_id,name FROM source_groups WHERE tenant_id=$1 AND connection_id=$2 AND source_group_native_id=$3`, tenantID, connectionID, nativeID).Scan(&out.TenantID, &out.ID, &out.ConnectionID, &out.NativeID, &out.Name)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return source.SourceGroup{}, source.ErrDenied
	}
	if err != nil {
		return source.SourceGroup{}, fmt.Errorf("resolve source group: %w", err)
	}
	return out, nil
}
