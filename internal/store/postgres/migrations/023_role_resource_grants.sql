-- 023_role_resource_grants (OA-04, D-25): roles carry explicit stored AI
-- resource grants. Deny by default; pre-023 platform.admin-scoped roles are
-- backfilled to mode:all so administrator authority survives as stored
-- representation instead of an implicit scope grant.
ALTER TABLE roles ADD COLUMN IF NOT EXISTS allowed_models          JSONB NOT NULL DEFAULT '{"mode":"list","items":[]}'::jsonb;
ALTER TABLE roles ADD COLUMN IF NOT EXISTS allowed_endpoints       JSONB NOT NULL DEFAULT '{"mode":"list","items":[]}'::jsonb;
ALTER TABLE roles ADD COLUMN IF NOT EXISTS allowed_assistants      JSONB NOT NULL DEFAULT '{"mode":"list","items":[]}'::jsonb;
ALTER TABLE roles ADD COLUMN IF NOT EXISTS allowed_extract_templates JSONB NOT NULL DEFAULT '{"mode":"list","items":[]}'::jsonb;

UPDATE roles
   SET allowed_models = '{"mode":"all"}'::jsonb,
       allowed_endpoints = '{"mode":"all"}'::jsonb,
       allowed_assistants = '{"mode":"all"}'::jsonb,
       allowed_extract_templates = '{"mode":"all"}'::jsonb
 WHERE scopes @> '"platform.admin"';
