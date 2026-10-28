-- 030_home_projections.sql — Home activity/attention read paths (08-09).
-- Read side only: jobs.progress decoding needs no column change.
-- idx_jobs_claim (status, lease_expires_at) serves worker claims only, so
-- both dashboard predicates get their own covering indexes.

-- Actor-scoped activity: member "Running now" plus own failed-extraction
-- attention, keyed on the frozen principal's actor ID.
CREATE INDEX idx_jobs_actor_status
    ON jobs (tenant_id, (principal->>'ActorID'), status, submitted_at DESC)
    WHERE deleted_at IS NULL;

-- Capability-managed tenant activity (manager views: ops × status).
CREATE INDEX idx_jobs_operation_status
    ON jobs (tenant_id, operation, status, submitted_at DESC)
    WHERE deleted_at IS NULL;
