-- Durable artifact references and the jobs/idempotency/lease/fence model
-- (CORE-03, JOB-01, JOB-02). Same conventions as 002/003: tenant-scoped
-- composite primary keys and composite foreign keys only.

-- An artifact row is the durable reference; storage_path is composed by the
-- server, never accepted from a caller. Begin inserts the 'writing' row plus
-- lease BEFORE bytes exist; Finalize/Reconcile verify the same row+fence so
-- an expired writer's bytes are invalidated before deletion and an in-flight
-- write is never orphaned.
CREATE TABLE artifacts (
    tenant_id          UUID NOT NULL REFERENCES tenants(id),
    id                 TEXT NOT NULL,
    kind               TEXT NOT NULL CHECK (kind IN ('original','result','derived')),
    version            INT NOT NULL DEFAULT 1,
    object_version_id  TEXT NULL,
    sha256             TEXT NOT NULL,
    byte_size          BIGINT NOT NULL,
    mime_type          TEXT NOT NULL DEFAULT '',
    original_filename  TEXT NOT NULL DEFAULT '',
    owner_actor_id     TEXT NULL,
    audience           JSONB NOT NULL DEFAULT '[]',
    restrictions       JSONB NOT NULL DEFAULT '{}',
    state              TEXT NOT NULL CHECK (state IN ('writing','final','deleting','tombstoned')),
    storage_path       TEXT NOT NULL,
    lease_owner        TEXT NULL,
    lease_expires_at   TIMESTAMPTZ NULL,
    fence_token        BIGINT NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    finalized_at       TIMESTAMPTZ NULL,
    deleted_at         TIMESTAMPTZ NULL,
    tombstone_reason   TEXT NULL,
    PRIMARY KEY (tenant_id, id),
    CHECK (state <> 'tombstoned' OR tombstone_reason IS NOT NULL),
    FOREIGN KEY (tenant_id, object_version_id)
        REFERENCES source_object_versions(tenant_id, id)
);

-- A job row freezes the operation, immutable input identity, submitting
-- principal and policy BEFORE a job ID is returned. submitter_key is the
-- canonical owner binding (tenant+actor+credential+delegation+billing
-- identity): idempotency replay/conflict compares it, so a same-tenant
-- different-owner resubmission of the same key is a conflict, never another
-- owner's job.
CREATE TABLE jobs (
    tenant_id           UUID NOT NULL REFERENCES tenants(id),
    id                  TEXT NOT NULL,
    operation           TEXT NOT NULL,
    status              TEXT NOT NULL CHECK (status IN ('queued','running','succeeded','failed','cancelled','unknown','deleted')),
    connection_id       TEXT NULL,
    object_id           TEXT NULL,
    observed_revision   BIGINT NULL,
    idempotency_key     TEXT NOT NULL,
    input_hash          TEXT NOT NULL,
    input_ref           TEXT NULL,
    principal           JSONB NOT NULL,       -- frozen full Principal
    submitter_key       TEXT NOT NULL,
    policy              JSONB NOT NULL DEFAULT '{}',
    attempts            INT NOT NULL DEFAULT 0,
    max_attempts        INT NOT NULL DEFAULT 3,
    fence_token         BIGINT NOT NULL DEFAULT 0,
    lease_owner         TEXT NULL,
    lease_expires_at    TIMESTAMPTZ NULL,
    dispatched_attempt_id TEXT NULL,          -- FK added after job_attempts exists
    progress            JSONB NOT NULL DEFAULT '{}',
    result_ref          TEXT NULL,
    error               JSONB NULL,
    submitted_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at          TIMESTAMPTZ NULL,
    finished_at         TIMESTAMPTZ NULL,
    cancel_requested_at TIMESTAMPTZ NULL,
    deleted_at          TIMESTAMPTZ NULL,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, operation, idempotency_key)
);
-- Subject dedup: find the job already covering one observed revision.
CREATE INDEX idx_jobs_subject
    ON jobs (tenant_id, connection_id, object_id, observed_revision);
-- Worker claims: queued/expired-lease jobs are the claimable set.
CREATE INDEX idx_jobs_claim ON jobs (status, lease_expires_at);

CREATE TABLE job_attempts (
    tenant_id     UUID NOT NULL,
    id            TEXT NOT NULL,
    job_id        TEXT NOT NULL,
    fence_token   BIGINT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('running','succeeded','failed','unknown')),
    worker_id     TEXT NOT NULL,
    dispatched_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at   TIMESTAMPTZ NULL,
    usage_ref     TEXT NULL,
    error         JSONB NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, job_id) REFERENCES jobs(tenant_id, id)
);
CREATE INDEX idx_job_attempts_job ON job_attempts (tenant_id, job_id);

-- The currently dispatched attempt of a job; a stale worker's attempt_id no
-- longer matches, so it cannot publish progress or results.
ALTER TABLE jobs
    ADD CONSTRAINT jobs_dispatched_attempt_fk
    FOREIGN KEY (tenant_id, dispatched_attempt_id)
    REFERENCES job_attempts(tenant_id, id);
