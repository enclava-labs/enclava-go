-- Retried syncs left a terminal rag.ingest/rag.embed job per superseded
-- observed revision (~240k rows) plus the source_object_versions rows those
-- jobs targeted. Jobs billing touched carry reservations and are kept.
-- jobs.dispatched_attempt_id points at job_attempts while job_attempts.job_id
-- points back, and both FKs are NO ACTION (checked at statement end), so the
-- attempts and the jobs must die inside one statement via a data-modifying
-- CTE.

-- The deletes check every referencing FK per deleted row; the unindexed
-- ones would scan the whole table for each row.
CREATE INDEX IF NOT EXISTS idx_reservations_job
    ON reservations (tenant_id, job_id) WHERE job_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_reservations_attempt
    ON reservations (tenant_id, attempt_id) WHERE attempt_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_jobs_dispatched_attempt
    ON jobs (tenant_id, dispatched_attempt_id) WHERE dispatched_attempt_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_index_generations_version
    ON index_generations (tenant_id, object_version_id);
CREATE INDEX IF NOT EXISTS idx_parsed_documents_version
    ON parsed_documents (tenant_id, object_version_id);
CREATE INDEX IF NOT EXISTS idx_artifacts_version
    ON artifacts (tenant_id, object_version_id) WHERE object_version_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_rag_evidence_version
    ON rag_evidence (tenant_id, object_version_id);

WITH doomed AS MATERIALIZED (
    SELECT j.tenant_id, j.id
    FROM jobs j
    WHERE j.operation IN ('rag.ingest', 'rag.embed')
      AND j.status IN ('succeeded','failed','cancelled','unknown','deleted')
      AND EXISTS (
          SELECT 1 FROM source_object_versions v
          WHERE v.tenant_id = j.tenant_id AND v.object_id = j.object_id
            AND v.observed_seq > j.observed_revision)
      AND NOT EXISTS (
          SELECT 1 FROM reservations r
          WHERE r.tenant_id = j.tenant_id AND r.job_id = j.id)
      AND NOT EXISTS (
          SELECT 1 FROM reservations r
          JOIN job_attempts a ON a.tenant_id = r.tenant_id AND a.id = r.attempt_id
          WHERE a.tenant_id = j.tenant_id AND a.job_id = j.id)
), attempts AS (
    DELETE FROM job_attempts a
    USING doomed d
    WHERE a.tenant_id = d.tenant_id AND a.job_id = d.id
)
DELETE FROM jobs j
USING doomed d
WHERE j.tenant_id = d.tenant_id AND j.id = d.id;

-- Fresh stats so the version delete hash-joins jobs instead of nested-looping
-- per row over statistics that still count the just-deleted jobs.
ANALYZE jobs;

-- Now the superseded version rows themselves: only versions that are not
-- their object's latest, have no derived data left (generations, chunks,
-- parsed documents, RAG evidence) and no remaining job row that pins them
-- (by input_ref or by subject revision) may go.
DELETE FROM source_object_versions v
WHERE EXISTS (
      SELECT 1 FROM source_object_versions v2
      WHERE v2.tenant_id = v.tenant_id AND v2.object_id = v.object_id
        AND v2.observed_seq > v.observed_seq)
  AND NOT EXISTS (
      SELECT 1 FROM index_generations g
      WHERE g.tenant_id = v.tenant_id AND g.object_version_id = v.id)
  AND NOT EXISTS (
      SELECT 1 FROM chunks k
      WHERE k.tenant_id = v.tenant_id AND k.object_version_id = v.id)
  AND NOT EXISTS (
      SELECT 1 FROM parsed_documents d
      WHERE d.tenant_id = v.tenant_id AND d.object_version_id = v.id)
  AND NOT EXISTS (
      SELECT 1 FROM rag_evidence e
      WHERE e.tenant_id = v.tenant_id AND e.object_version_id = v.id)
  AND NOT EXISTS (
      SELECT 1 FROM jobs j
      WHERE j.tenant_id = v.tenant_id AND j.input_ref = v.id)
  AND NOT EXISTS (
      SELECT 1 FROM jobs j
      WHERE j.tenant_id = v.tenant_id
        AND j.object_id = v.object_id AND j.observed_revision = v.observed_seq);
