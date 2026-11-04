package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"enclava-go/internal/source"
)

// GetObjects reads many source_objects rows in one query — the same
// projection GetObject scans per row — keyed by object id. Missing ids are
// absent from the map; callers apply GetObject's not-found outcome to each.
func (r *RAGRepo) GetObjects(ctx context.Context, tenantID string, ids []string) (map[string]source.Object, error) {
	out := make(map[string]source.Object, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT tenant_id::text, id, connection_id, kind, native_id, parent_object_id,
		       authorization_mode, owner_actor_id, audience, upstream_revision,
		       observed_seq, deleted_at, tombstoned_at
		FROM source_objects WHERE tenant_id = $1 AND id = ANY($2)`,
		tenantID, ids)
	if err != nil {
		if isInvalidUUID(err) {
			// GetObject answers malformed tenant ids with its not-found
			// outcome — an empty map denies each caller-side document the
			// same way.
			return out, nil
		}
		return nil, fmt.Errorf("get objects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var o source.Object
		var connID, parent, owner sql.NullString
		var audience []byte
		var upstream sql.NullString
		var deleted, tombstoned sql.NullTime
		if err := rows.Scan(
			&o.TenantID, &o.ID, &connID, &o.Kind, &o.NativeID, &parent,
			&o.AuthorizationMode, &owner, &audience, &upstream,
			&o.ObservedSeq, &deleted, &tombstoned); err != nil {
			return nil, fmt.Errorf("scan object: %w", err)
		}
		o.ConnectionID = connID.String
		o.ParentObjectID = parent.String
		o.OwnerActorID = owner.String
		o.Audience = scanJSONList(audience)
		if upstream.Valid {
			s := upstream.String
			o.UpstreamRevision = &s
		}
		if deleted.Valid {
			t := deleted.Time
			o.DeletedAt = &t
		}
		if tombstoned.Valid {
			t := tombstoned.Time
			o.TombstonedAt = &t
		}
		out[o.ID] = o
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
