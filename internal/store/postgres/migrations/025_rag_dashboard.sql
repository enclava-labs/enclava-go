-- 08-03: document tombstone metadata + failed-status/list indexes.
-- `removed_at` is the soft-delete marker for collection membership: the row
-- stays for management metadata while every member read-path excludes it.

ALTER TABLE collection_items
    ADD COLUMN removed_at TIMESTAMPTZ NULL;

-- Authorized per-collection listing (all/ready/processing/failed exclude
-- removed rows; removed reads the marker directly).
CREATE INDEX idx_collection_items_list
    ON collection_items (tenant_id, collection_id, removed_at);

-- Failed-status projection: document rows join their durable rag.embed job
-- on input_ref (the object version) for the newest generation. The claim
-- index (status, lease_expires_at) does not cover these lookups.
CREATE INDEX idx_jobs_failed_input
    ON jobs (tenant_id, operation, input_ref) WHERE status = 'failed';
