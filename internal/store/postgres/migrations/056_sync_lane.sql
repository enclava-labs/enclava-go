-- Connector syncs get their own lane: a sync never waits behind bulk
-- embedding work and never holds a slot interactive jobs need.
ALTER TABLE jobs DROP CONSTRAINT jobs_lane_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_lane_check
    CHECK (lane IN ('interactive', 'sync', 'background'));

UPDATE jobs SET lane = 'sync'
WHERE operation = 'connector.sync' AND status IN ('queued', 'running');
