-- Canonical source schema (CORE-04 / source §Storage and identity):
-- connections, objects, immutable observed versions, source identities,
-- groups, revision-qualified memberships and grants, access snapshots and
-- server-owned restrictions.

CREATE TABLE connections (
    tenant_id            UUID NOT NULL REFERENCES tenants(id),
    id                   TEXT NOT NULL,
    kind                 TEXT NOT NULL CHECK (kind IN ('github','slack','teams','notion','google_drive')),
    name                 TEXT NOT NULL,
    credential_ref       TEXT NOT NULL,
    config               JSONB NOT NULL DEFAULT '{}',
    target_collection_id TEXT NULL,
    audience_default     JSONB NOT NULL DEFAULT '[]',
    status               TEXT NOT NULL DEFAULT 'active',
    created_by           TEXT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    synced_at            TIMESTAMPTZ NULL,
    sync_status          TEXT NULL,
    sync_error           TEXT NULL,
    PRIMARY KEY (tenant_id, id)
);

-- Source identity is (tenant_id, connection_id, kind, native_id). A NULL
-- connection_id is a tenant-owned upload — a DISTINCT scope, so uploads
-- dedupe via the partial unique index instead of the plain unique
-- constraint (Postgres treats NULLs as distinct).
CREATE TABLE source_objects (
    tenant_id           UUID NOT NULL REFERENCES tenants(id),
    id                  TEXT NOT NULL,
    connection_id       TEXT NULL,
    kind                TEXT NOT NULL,
    native_id           TEXT NOT NULL,
    parent_object_id    TEXT NULL,
    authorization_mode  TEXT NOT NULL CHECK (authorization_mode IN ('owner','audience','source_acl','published_audience')),
    owner_actor_id      TEXT NULL,
    audience            JSONB NOT NULL DEFAULT '[]',
    upstream_revision   TEXT NULL,
    observed_seq        BIGINT NOT NULL,
    source_modified_at  TIMESTAMPTZ NULL,
    first_seen_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at          TIMESTAMPTZ NULL,
    tombstoned_at       TIMESTAMPTZ NULL,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, connection_id, kind, native_id),
    FOREIGN KEY (tenant_id, connection_id) REFERENCES connections(tenant_id, id),
    FOREIGN KEY (tenant_id, parent_object_id) REFERENCES source_objects(tenant_id, id)
);
-- Uploads (connection_id IS NULL) dedupe on (tenant, kind, native_id).
CREATE UNIQUE INDEX idx_source_objects_upload_identity
    ON source_objects (tenant_id, kind, native_id)
    WHERE connection_id IS NULL;
CREATE INDEX idx_source_objects_connection ON source_objects(tenant_id, connection_id, kind);

-- Insert-only immutable observations. There is deliberately no mutable
-- status column: identical bytes at a new observed_seq are a NEW row, so
-- an A->B->A transition can never collapse into old work.
CREATE TABLE source_object_versions (
    tenant_id          UUID NOT NULL REFERENCES tenants(id),
    id                 TEXT NOT NULL,
    object_id          TEXT NOT NULL,
    observed_seq       BIGINT NOT NULL,
    artifact_ref       TEXT NOT NULL,
    content_sha256     TEXT NOT NULL,
    byte_size          BIGINT NOT NULL,
    mime_type          TEXT NOT NULL DEFAULT '',
    original_filename  TEXT NOT NULL DEFAULT '',
    upstream_revision  TEXT NULL,
    provenance         JSONB NOT NULL DEFAULT '{}',
    restrictions       JSONB NOT NULL DEFAULT '{}',
    created_by         TEXT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, object_id, observed_seq),
    FOREIGN KEY (tenant_id, object_id) REFERENCES source_objects(tenant_id, id)
);
CREATE INDEX idx_source_object_versions_object
    ON source_object_versions(tenant_id, object_id, observed_seq DESC);

CREATE TABLE source_identities (
    tenant_id            UUID NOT NULL REFERENCES tenants(id),
    id                   TEXT NOT NULL,
    connection_id        TEXT NOT NULL,
    source_user_native_id TEXT NOT NULL,
    display_name         TEXT NOT NULL DEFAULT '',
    email                TEXT NULL,
    mapped_user_id       TEXT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, connection_id, source_user_native_id),
    FOREIGN KEY (tenant_id, connection_id) REFERENCES connections(tenant_id, id),
    FOREIGN KEY (tenant_id, mapped_user_id) REFERENCES users(tenant_id, id)
);

CREATE TABLE source_groups (
    tenant_id              UUID NOT NULL REFERENCES tenants(id),
    id                     TEXT NOT NULL,
    connection_id          TEXT NOT NULL,
    source_group_native_id TEXT NOT NULL,
    name                   TEXT NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, connection_id, source_group_native_id),
    FOREIGN KEY (tenant_id, connection_id) REFERENCES connections(tenant_id, id)
);

-- Revision-qualified memberships: each row records the access_snapshots
-- revision under which the membership was observed. Authoritative reads
-- filter on revisions whose snapshot is status='complete', so an
-- incomplete replacement revision can never shadow the last complete set.
CREATE TABLE source_group_memberships (
    tenant_id          UUID NOT NULL REFERENCES tenants(id),
    id                 TEXT NOT NULL,
    group_id           TEXT NOT NULL,
    source_identity_id TEXT NOT NULL,
    access_revision    BIGINT NOT NULL,
    observed_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, group_id) REFERENCES source_groups(tenant_id, id),
    FOREIGN KEY (tenant_id, source_identity_id) REFERENCES source_identities(tenant_id, id)
);
CREATE INDEX idx_source_group_memberships_group_rev
    ON source_group_memberships(tenant_id, group_id, access_revision);

CREATE TABLE source_grants (
    tenant_id          UUID NOT NULL REFERENCES tenants(id),
    id                 TEXT NOT NULL,
    object_id          TEXT NOT NULL,
    subject_type       TEXT NOT NULL CHECK (subject_type IN ('user','group')),
    source_identity_id TEXT NULL,
    source_group_id    TEXT NULL,
    audience           JSONB NOT NULL DEFAULT '[]',
    effect             TEXT NOT NULL CHECK (effect IN ('allow','deny')),
    access_revision    BIGINT NOT NULL,
    observed_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, object_id) REFERENCES source_objects(tenant_id, id),
    FOREIGN KEY (tenant_id, source_identity_id) REFERENCES source_identities(tenant_id, id),
    FOREIGN KEY (tenant_id, source_group_id) REFERENCES source_groups(tenant_id, id)
);
CREATE INDEX idx_source_grants_object_rev
    ON source_grants(tenant_id, object_id, access_revision);

-- Snapshot bookkeeping. status='incomplete' marks partial pagination and is
-- never authoritative; only status='complete' revisions may be used by
-- authorizer reads.
CREATE TABLE access_snapshots (
    tenant_id     UUID NOT NULL REFERENCES tenants(id),
    id            TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('content','grants','membership')),
    revision      BIGINT NOT NULL,
    completed_at  TIMESTAMPTZ NULL,
    status        TEXT NOT NULL CHECK (status IN ('incomplete','complete')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, connection_id, kind, revision),
    FOREIGN KEY (tenant_id, connection_id) REFERENCES connections(tenant_id, id)
);

CREATE TABLE source_restrictions (
    tenant_id           UUID NOT NULL REFERENCES tenants(id),
    id                  TEXT NOT NULL,
    subject_type        TEXT NOT NULL CHECK (subject_type IN ('connection','object','artifact')),
    subject_id          TEXT NOT NULL,
    required_assurance  TEXT NULL,
    denied_capabilities JSONB NOT NULL DEFAULT '[]',
    policy_labels       JSONB NOT NULL DEFAULT '[]',
    set_by_actor_id     TEXT NULL,
    set_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX idx_source_restrictions_subject
    ON source_restrictions(tenant_id, subject_type, subject_id);
