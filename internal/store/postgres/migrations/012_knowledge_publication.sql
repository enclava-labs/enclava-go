ALTER TABLE index_generations
    ADD COLUMN parser_version TEXT NOT NULL DEFAULT '',
    ADD COLUMN chunk_profile_version_id TEXT NULL,
    ADD COLUMN embedding_model_version TEXT NOT NULL DEFAULT '',
    ADD COLUMN artifact_ref TEXT NOT NULL DEFAULT '',
    ADD COLUMN access_revision BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN lease_fence BIGINT NOT NULL DEFAULT 0;

ALTER TABLE chunks
    ADD COLUMN collection_id TEXT NULL,
    ADD COLUMN generation BIGINT NOT NULL DEFAULT 1,
    ADD COLUMN parent_chunk_id TEXT NULL,
    ADD COLUMN block_path TEXT NOT NULL DEFAULT '',
    ADD COLUMN locator JSONB NOT NULL DEFAULT '{}',
    ADD COLUMN qdrant_point_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_chunks_generation ON chunks (tenant_id, collection_id, object_version_id, generation, ordinal);

CREATE TABLE index_reconciliation (
    tenant_id UUID NOT NULL REFERENCES tenants(id),
    collection_id TEXT NOT NULL,
    object_version_id TEXT NOT NULL,
    generation BIGINT NOT NULL,
    fence_token BIGINT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('pending','complete')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, collection_id, object_version_id, generation),
    FOREIGN KEY (tenant_id, collection_id, object_version_id, generation)
        REFERENCES index_generations(tenant_id, collection_id, object_version_id, generation)
);

CREATE TABLE knowledge_imports (
    tenant_id UUID NOT NULL REFERENCES tenants(id),
    collection_id TEXT NOT NULL,
    import_ref TEXT NOT NULL,
    object_id TEXT NOT NULL,
    provenance_kind TEXT NOT NULL CHECK (provenance_kind IN ('upload','generated')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, collection_id, import_ref),
    FOREIGN KEY (tenant_id, collection_id) REFERENCES knowledge_collections(tenant_id, id),
    FOREIGN KEY (tenant_id, object_id) REFERENCES source_objects(tenant_id, id)
);
