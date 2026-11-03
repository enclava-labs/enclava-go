-- Upload dedupe (KC-I04): a re-upload of identical bytes into the same
-- collection must find the live document by its current version's content
-- hash. This index keeps that lookup from scanning every version row.
CREATE INDEX idx_source_object_versions_sha256
    ON source_object_versions(tenant_id, content_sha256);
