package postgres

import (
	"context"
	"fmt"
	"slices"
)

// ancestorChainRow is one recursive-CTE row for AncestorObjectIDsBatch.
type ancestorChainRow struct {
	rootID string
	id     string
	depth  int
	valid  bool
}

// AncestorObjectIDsBatch resolves the complete, valid parent chain of every
// requested object in one query — the same recursive walk, liveness rules
// and completeness checks AncestorObjectIDs applies per object, batched over
// the whole set. An object whose chain is missing, cyclic or over-depth is
// absent from the returned map, which the authorizer denies like the
// per-object error.
func (r *SourceRepo) AncestorObjectIDsBatch(ctx context.Context, tenantID, connectionID string, objectIDs []string, maxDepth int) (map[string][]string, error) {
	out := make(map[string][]string, len(objectIDs))
	if maxDepth < 0 || len(objectIDs) == 0 {
		return out, nil
	}
	rows, err := r.db.SQL.QueryContext(ctx, `
		WITH RECURSIVE ancestors AS (
			SELECT s.root_id, s.root_id AS id, o.parent_object_id, 0 AS depth,
			       (o.id IS NOT NULL AND o.deleted_at IS NULL AND o.tombstoned_at IS NULL) AS valid
			FROM (SELECT DISTINCT id AS root_id FROM unnest($3::text[]) AS u(id)) s
			LEFT JOIN source_objects o ON o.tenant_id = $1 AND o.connection_id = $2 AND o.id = s.root_id
			UNION ALL
			SELECT a.root_id, a.parent_object_id, o.parent_object_id, a.depth + 1,
			       (o.id IS NOT NULL AND o.deleted_at IS NULL AND o.tombstoned_at IS NULL)
			FROM ancestors a
			LEFT JOIN source_objects o ON o.tenant_id = $1 AND o.connection_id = $2 AND o.id = a.parent_object_id
			WHERE a.valid AND a.parent_object_id IS NOT NULL AND a.depth <= $4
		)
		SELECT root_id, id, depth, valid FROM ancestors`, tenantID, connectionID, objectIDs, maxDepth)
	if err != nil {
		return nil, fmt.Errorf("ancestor objects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	byRoot := make(map[string][]ancestorChainRow, len(objectIDs))
	for rows.Next() {
		var row ancestorChainRow
		if err := rows.Scan(&row.rootID, &row.id, &row.depth, &row.valid); err != nil {
			return nil, fmt.Errorf("scan ancestor: %w", err)
		}
		byRoot[row.rootID] = append(byRoot[row.rootID], row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for rootID, chainRows := range byRoot {
		// The chain is linear; depth order restores AncestorObjectIDs'
		// scan order (self first, root last).
		slices.SortFunc(chainRows, func(a, b ancestorChainRow) int { return a.depth - b.depth })
		ids := []string{}
		seen := map[string]struct{}{}
		complete := true
		for _, row := range chainRows {
			if _, exists := seen[row.id]; !row.valid || row.depth > maxDepth || exists {
				complete = false
				break
			}
			seen[row.id] = struct{}{}
			ids = append(ids, row.id)
		}
		if complete {
			out[rootID] = ids
		}
	}
	return out, nil
}
