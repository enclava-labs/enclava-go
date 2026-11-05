-- Failed indexing retried as new versions left generations behind that never
-- wrote index points (unbound: binding happens right before the upsert),
-- never published and belong to an older version of their object. Nothing
-- reads them; drop them (their reconciliation rows cascade) unless an
-- attempt is still queued or running or chunks were staged for them.
DELETE FROM index_generations g
USING source_object_versions v, source_objects o
WHERE v.tenant_id = g.tenant_id AND v.id = g.object_version_id
  AND o.tenant_id = v.tenant_id AND o.id = v.object_id
  AND v.observed_seq < o.observed_seq
  AND g.qdrant_collection = ''
  AND g.published_at IS NULL
  AND g.state IN ('staging', 'superseded', 'failed')
  AND NOT EXISTS (
      SELECT 1 FROM chunks k
      WHERE k.tenant_id = g.tenant_id AND k.collection_id = g.collection_id
        AND k.object_version_id = g.object_version_id AND k.generation = g.generation)
  AND NOT EXISTS (
      SELECT 1 FROM jobs j
      WHERE j.tenant_id = g.tenant_id AND j.status IN ('queued', 'running')
        AND j.input_ref = g.object_version_id
        AND j.policy->>'generation' = g.generation::text);

-- Published-generation listings and per-state lookups filter by collection
-- and state; the collection prefix alone matches nearly every row.
CREATE INDEX index_generations_collection_state_idx
    ON index_generations (tenant_id, collection_id, state);

-- Workers claim pending reconciliations oldest first.
CREATE INDEX index_reconciliation_pending_idx
    ON index_reconciliation (tenant_id, created_at) WHERE state = 'pending';
