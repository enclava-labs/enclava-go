-- Job lanes: bulk background work (connector syncs and their ingest/embed
-- fan-out, access projections, re-embed scans) is claimed by its own runner
-- so it can never starve interactive jobs (uploads, extraction, agent runs).
ALTER TABLE jobs ADD COLUMN lane TEXT NOT NULL DEFAULT 'interactive'
    CHECK (lane IN ('interactive', 'background'));

UPDATE jobs SET lane = 'background'
WHERE status IN ('queued', 'running')
  AND (connection_id IS NOT NULL
       OR operation IN ('connector.sync', 'rag.access', 'rag.access_backfill', 'rag.reembed_scan'));

CREATE INDEX idx_jobs_lane_claim ON jobs (lane, status, submitted_at);
