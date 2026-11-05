package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"enclava-go/internal/source"
)

// CollectionsForObjects returns, per object ID, every live collection
// (removed_at IS NULL) containing it — the read set AuthorizationCollection
// evaluates. Objects in no collection are absent from the map.
func (r *SourceRepo) CollectionsForObjects(ctx context.Context, tenantID string, objectIDs []string) (map[string][]source.CollectionAccess, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT ci.object_id, c.id, c.owner_actor_id, c.audience, c.is_active
		FROM collection_items ci
		JOIN knowledge_collections c
		  ON c.tenant_id = ci.tenant_id AND c.id = ci.collection_id
		WHERE ci.tenant_id = $1 AND ci.object_id = ANY($2) AND ci.removed_at IS NULL`,
		tenantID, objectIDs)
	if err != nil {
		return nil, fmt.Errorf("collections for objects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]source.CollectionAccess{}
	for rows.Next() {
		var objectID string
		var access source.CollectionAccess
		var owner sql.NullString
		var audience []byte
		if err := rows.Scan(&objectID, &access.ID, &owner, &audience, &access.IsActive); err != nil {
			return nil, fmt.Errorf("scan collection access: %w", err)
		}
		access.OwnerActorID = owner.String
		access.Audience = scanJSONList(audience)
		out[objectID] = append(out[objectID], access)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
