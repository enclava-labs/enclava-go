-- Search access projection: index points now carry acl tokens and a
-- published flag written when the generation is embedded/published.
-- access_projected marks generations whose points already carry the
-- projection; rows predating this change stay FALSE until the
-- rag.access_backfill job rewrites their points.
ALTER TABLE index_generations
    ADD COLUMN access_projected BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN acl_revision BIGINT NOT NULL DEFAULT 0;

CREATE INDEX index_generations_unprojected_idx
    ON index_generations (tenant_id, id)
    WHERE state = 'published' AND NOT access_projected;
