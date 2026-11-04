// source_progress_repo.go — one aggregate read per connection for the
// Sources table: the same live-item and latest-version/latest-generation
// semantics documentSelect applies, folded into COUNT buckets instead of
// per-document rows.
package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"enclava-go/internal/rag"
	"enclava-go/internal/source"
)

// sourceIndexProgressQuery mirrors documentStatusPredicate exactly: live
// items only (no removed membership, deleted or tombstoned object), the
// latest version by observed_seq, the latest index generation per
// (collection, version). Failed = a failed rag.embed/rag.ingest job for the
// latest generation's version+generation, or generation state 'failed';
// indexed = published and not failed; the rest is processing. The reason
// block picks the failed jobs' most common error message over failed items
// and the items carrying it.
const sourceIndexProgressQuery = `
	WITH item_status AS (
		SELECT COALESCE(lg.state, 'staging') AS gen_state,
		       (fj.id IS NOT NULL OR lg.state = 'failed') AS failed,
		       fj.message
		FROM source_objects o
		JOIN collection_items ci
		  ON ci.tenant_id = o.tenant_id AND ci.object_id = o.id
		 AND ci.collection_id = $3 AND ci.removed_at IS NULL
		LEFT JOIN LATERAL (
			SELECT v.id
			FROM source_object_versions v
			WHERE v.tenant_id = o.tenant_id AND v.object_id = o.id
			ORDER BY v.observed_seq DESC
			LIMIT 1
		) lv ON true
		LEFT JOIN LATERAL (
			SELECT g.object_version_id, g.generation, g.state
			FROM index_generations g
			WHERE g.tenant_id = o.tenant_id AND g.collection_id = $3
			  AND g.object_version_id = lv.id
			ORDER BY g.generation DESC
			LIMIT 1
		) lg ON true
		LEFT JOIN LATERAL (
			SELECT j.id, j.error->>'message' AS message
			FROM jobs j
			WHERE j.tenant_id = o.tenant_id
			  AND j.operation IN ('` + rag.OperationEmbed + `', '` + rag.OperationIngest + `')
			  AND j.input_ref = lg.object_version_id
			  AND j.policy->>'generation' = lg.generation::text
			  AND j.status = 'failed'
			ORDER BY j.finished_at DESC NULLS LAST
			LIMIT 1
		) fj ON true
		WHERE o.tenant_id = $1 AND o.connection_id = $2
		  AND o.deleted_at IS NULL AND o.tombstoned_at IS NULL
	),
	reasons AS (
		SELECT message, COUNT(*) AS n
		FROM item_status
		WHERE failed AND message IS NOT NULL
		GROUP BY message
		ORDER BY n DESC, message
		LIMIT 1
	)
	SELECT COUNT(*),
	       COUNT(*) FILTER (WHERE gen_state = 'published' AND NOT failed),
	       COUNT(*) FILTER (WHERE failed),
	       COUNT(*) FILTER (WHERE NOT failed AND gen_state <> 'published'),
	       (SELECT message FROM reasons), (SELECT n FROM reasons)
	FROM item_status`

// SourceIndexProgress aggregates one connection's live collection items into
// indexed/failed/processing buckets for connectors.Service.IndexProgress.
func (r *SourceRepo) SourceIndexProgress(ctx context.Context, tenantID, connectionID, collectionID string) (source.IndexProgress, error) {
	var p source.IndexProgress
	var message sql.NullString
	var messageCount sql.NullInt64
	err := r.db.SQL.QueryRowContext(ctx, sourceIndexProgressQuery, tenantID, connectionID, collectionID).
		Scan(&p.Items, &p.Indexed, &p.Failed, &p.Processing, &message, &messageCount)
	if err != nil {
		return source.IndexProgress{}, fmt.Errorf("source index progress: %w", err)
	}
	p.FailureReason = message.String
	p.FailureCount = messageCount.Int64
	return p, nil
}
