-- Tenant-scoped domain schema (CORE-02): extract, prompts, chatbot,
-- knowledge collections and versioned chunk profiles. These replace the
-- legacy global/APIKeyID-owned tables — every table carries tenant_id plus
-- owner_actor_id and/or audience, never a global or key-owned row.

CREATE TABLE extract_templates (
    tenant_id        UUID NOT NULL REFERENCES tenants(id),
    id               TEXT NOT NULL,
    name             TEXT NOT NULL,
    description      TEXT NOT NULL DEFAULT '',
    system_prompt    TEXT NOT NULL DEFAULT '',
    user_prompt      TEXT NOT NULL DEFAULT '',
    variables_schema JSONB NOT NULL DEFAULT '{}',
    output_schema    JSONB NOT NULL DEFAULT '{}',
    default_model    TEXT NOT NULL DEFAULT '',
    enabled          BOOLEAN NOT NULL DEFAULT true,
    owner_actor_id   TEXT NULL,
    is_default       BOOLEAN NOT NULL DEFAULT false,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name)
);

-- One settings row per tenant, keyed by the literal id 'default'.
CREATE TABLE extract_settings (
    tenant_id        UUID NOT NULL REFERENCES tenants(id),
    id               TEXT NOT NULL CHECK (id = 'default'),
    default_model    TEXT NOT NULL DEFAULT '',
    max_file_size_mb BIGINT NOT NULL DEFAULT 0,
    owner_actor_id   TEXT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

CREATE TABLE prompt_templates (
    tenant_id        UUID NOT NULL REFERENCES tenants(id),
    id               TEXT NOT NULL,
    name             TEXT NOT NULL,
    description      TEXT NOT NULL DEFAULT '',
    system_prompt    TEXT NOT NULL DEFAULT '',
    user_prompt      TEXT NOT NULL DEFAULT '',
    variables_schema JSONB NOT NULL DEFAULT '{}',
    is_default       BOOLEAN NOT NULL DEFAULT false,
    is_active        BOOLEAN NOT NULL DEFAULT true,
    owner_actor_id   TEXT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name)
);

CREATE TABLE chatbots (
    tenant_id          UUID NOT NULL REFERENCES tenants(id),
    id                 TEXT NOT NULL,
    external_id        TEXT NOT NULL,
    name               TEXT NOT NULL,
    description        TEXT NOT NULL DEFAULT '',
    prompt_template_id TEXT NULL,
    endpoint_id        TEXT NULL,
    model              TEXT NOT NULL DEFAULT '',
    collection_ids     JSONB NOT NULL DEFAULT '[]',
    settings           JSONB NOT NULL DEFAULT '{}',
    is_enabled         BOOLEAN NOT NULL DEFAULT true,
    owner_actor_id     TEXT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, external_id),
    FOREIGN KEY (tenant_id, prompt_template_id)
        REFERENCES prompt_templates(tenant_id, id)
);

CREATE TABLE conversations (
    tenant_id      UUID NOT NULL REFERENCES tenants(id),
    id             TEXT NOT NULL,
    chatbot_id     TEXT NOT NULL,
    owner_actor_id TEXT NOT NULL,
    external_id    TEXT NULL,
    restrictions   JSONB NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, chatbot_id) REFERENCES chatbots(tenant_id, id)
);

CREATE TABLE messages (
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    id              TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    role            TEXT NOT NULL,
    content         TEXT NOT NULL,
    refs            JSONB NOT NULL DEFAULT '[]',
    restrictions    JSONB NOT NULL DEFAULT '{}',
    owner_actor_id  TEXT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, conversation_id)
        REFERENCES conversations(tenant_id, id)
);
CREATE INDEX idx_messages_conversation ON messages (tenant_id, conversation_id, created_at);

-- CHUNK PROFILE VERSIONING — explicit exception to the plain
-- (tenant_id,id)-per-entity convention. Each row is ONE immutable version:
-- updates INSERT a new row with the next version, never UPDATE in place.
-- source.ChunkProfile.ID identifies one immutable version row; ProfileID its
-- family; consumers bind chunk_profile_version_id, never the family ID.
CREATE TABLE chunk_profiles (
    tenant_id           UUID NOT NULL REFERENCES tenants(id),
    id                  TEXT NOT NULL,
    profile_id          TEXT NOT NULL,
    version             INT NOT NULL,
    name                TEXT NOT NULL,
    boundary_strategy   TEXT NOT NULL,
    max_size_codepoints BIGINT NOT NULL CHECK (max_size_codepoints > 0),
    overlap_codepoints  BIGINT NOT NULL DEFAULT 0 CHECK (overlap_codepoints >= 0),
    include_parent      BOOLEAN NOT NULL DEFAULT false,
    table_handling      TEXT NOT NULL DEFAULT 'row-wise',
    is_active           BOOLEAN NOT NULL DEFAULT true,
    created_by          TEXT NULL,
    owner_actor_id      TEXT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, profile_id, version)
);

CREATE TABLE knowledge_collections (
    tenant_id                UUID NOT NULL REFERENCES tenants(id),
    id                       TEXT NOT NULL,
    name                     TEXT NOT NULL,
    description              TEXT NOT NULL DEFAULT '',
    embedding_model          TEXT NOT NULL DEFAULT '',
    qdrant_collection        TEXT NOT NULL,
    chunk_profile_version_id TEXT NULL,
    audience                 JSONB NOT NULL DEFAULT '[]',
    metadata                 JSONB NOT NULL DEFAULT '{}',
    owner_actor_id           TEXT NULL,
    is_active                BOOLEAN NOT NULL DEFAULT true,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name),
    -- binds ONE immutable version row, never a floating family ID
    FOREIGN KEY (tenant_id, chunk_profile_version_id)
        REFERENCES chunk_profiles(tenant_id, id)
);

CREATE TABLE collection_items (
    tenant_id     UUID NOT NULL REFERENCES tenants(id),
    id            TEXT NOT NULL,
    collection_id TEXT NOT NULL,
    object_id     TEXT NOT NULL,
    added_by      TEXT NULL,
    owner_actor_id TEXT NULL,
    added_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, collection_id, object_id),
    FOREIGN KEY (tenant_id, collection_id)
        REFERENCES knowledge_collections(tenant_id, id),
    FOREIGN KEY (tenant_id, object_id)
        REFERENCES source_objects(tenant_id, id)
);

-- One parsed representation of a source object version. Re-parse with a new
-- parser_version inserts a new row; it never mutates parsed evidence.
CREATE TABLE parsed_documents (
    tenant_id         UUID NOT NULL REFERENCES tenants(id),
    id                TEXT NOT NULL,
    object_version_id TEXT NOT NULL,
    parser            TEXT NOT NULL,
    parser_version    TEXT NOT NULL,
    block_count       BIGINT NOT NULL DEFAULT 0,
    owner_actor_id    TEXT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, object_version_id)
        REFERENCES source_object_versions(tenant_id, id)
);

CREATE TABLE blocks (
    tenant_id          UUID NOT NULL REFERENCES tenants(id),
    id                 TEXT NOT NULL,
    parsed_document_id TEXT NOT NULL,
    ordinal            INT NOT NULL,
    kind               TEXT NOT NULL,
    parent_id          TEXT NULL,
    text               TEXT NOT NULL,
    byte_start         BIGINT NOT NULL,
    byte_end           BIGINT NOT NULL,
    locator            JSONB NOT NULL DEFAULT '{}',
    owner_actor_id     TEXT NULL,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, parsed_document_id, ordinal),
    FOREIGN KEY (tenant_id, parsed_document_id)
        REFERENCES parsed_documents(tenant_id, id),
    FOREIGN KEY (tenant_id, parent_id)
        REFERENCES blocks(tenant_id, id)
);

-- Chunks are pinned to the exact immutable chunk_profile version used to
-- produce them — never to the floating profile family.
CREATE TABLE chunks (
    tenant_id                UUID NOT NULL REFERENCES tenants(id),
    id                       TEXT NOT NULL,
    object_version_id        TEXT NOT NULL,
    chunk_profile_version_id TEXT NOT NULL,
    ordinal                  INT NOT NULL,
    text                     TEXT NOT NULL,
    byte_start               BIGINT NOT NULL,
    byte_end                 BIGINT NOT NULL,
    span_refs                JSONB NOT NULL DEFAULT '[]',
    owner_actor_id           TEXT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, object_version_id)
        REFERENCES source_object_versions(tenant_id, id),
    FOREIGN KEY (tenant_id, chunk_profile_version_id)
        REFERENCES chunk_profiles(tenant_id, id)
);
CREATE INDEX idx_chunks_version ON chunks (tenant_id, object_version_id, ordinal);

-- Index/publication generations: a staged generation publishes only when all
-- required artifacts, authorization and index writes are ready; failure
-- keeps the last published generation usable.
CREATE TABLE index_generations (
    tenant_id         UUID NOT NULL REFERENCES tenants(id),
    id                TEXT NOT NULL,
    collection_id     TEXT NOT NULL,
    object_version_id TEXT NOT NULL,
    generation        BIGINT NOT NULL,
    state             TEXT NOT NULL CHECK (state IN ('staging','published','superseded','failed')),
    published_at      TIMESTAMPTZ NULL,
    owner_actor_id    TEXT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, collection_id, object_version_id, generation),
    FOREIGN KEY (tenant_id, collection_id)
        REFERENCES knowledge_collections(tenant_id, id),
    FOREIGN KEY (tenant_id, object_version_id)
        REFERENCES source_object_versions(tenant_id, id)
);
