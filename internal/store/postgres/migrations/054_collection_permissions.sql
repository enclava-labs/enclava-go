-- Permissions are now per collection. Connector objects and uploaded
-- collection documents are readable by exactly the principals who can read
-- a collection containing them; source-native ACL modes are converted.

ALTER TABLE source_objects
    DROP CONSTRAINT source_objects_authorization_mode_check;

-- Every connector object is collection-scoped.
UPDATE source_objects SET authorization_mode = 'collection'
    WHERE connection_id IS NOT NULL;

-- Audience-mode uploads that already sit in a live collection item become
-- collection-scoped; anything else left in a removed mode converts too —
-- to 'collection' when it is a live collection item, 'owner' otherwise.
UPDATE source_objects o SET authorization_mode =
    CASE WHEN EXISTS (
        SELECT 1 FROM collection_items ci
        WHERE ci.tenant_id = o.tenant_id AND ci.object_id = o.id
          AND ci.removed_at IS NULL
    ) THEN 'collection' ELSE 'owner' END
    WHERE o.authorization_mode <> 'owner'
      AND (o.authorization_mode = 'audience'
        OR o.authorization_mode = 'source_acl'
        OR o.authorization_mode = 'published_audience');

ALTER TABLE source_objects
    ADD CONSTRAINT source_objects_authorization_mode_check
    CHECK (authorization_mode IN ('owner','collection'));

-- Converted objects keep their old search tokens until the access backfill
-- rewrites them; mark their generations unprojected and reset the source
-- grant revision their tokens were computed at.
UPDATE index_generations g SET access_projected = false, acl_revision = 0
    FROM source_object_versions v
    JOIN source_objects o ON o.tenant_id = v.tenant_id AND o.id = v.object_id
    WHERE g.tenant_id = v.tenant_id AND g.object_version_id = v.id
      AND o.authorization_mode = 'collection';
