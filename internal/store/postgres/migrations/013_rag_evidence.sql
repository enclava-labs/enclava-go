CREATE TABLE rag_evidence (
    tenant_id UUID NOT NULL REFERENCES tenants(id),
    id TEXT NOT NULL,
    collection_id TEXT NOT NULL,
    object_id TEXT NOT NULL,
    object_version_id TEXT NOT NULL,
    observed_seq BIGINT NOT NULL,
    generation BIGINT NOT NULL,
    chunk_id TEXT NOT NULL,
    span_refs JSONB NOT NULL DEFAULT '[]',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, collection_id) REFERENCES knowledge_collections(tenant_id, id),
    FOREIGN KEY (tenant_id, object_id) REFERENCES source_objects(tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, object_version_id) REFERENCES source_object_versions(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX idx_rag_evidence_object_version ON rag_evidence (tenant_id, object_id, object_version_id, generation);
