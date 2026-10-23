ALTER TABLE knowledge_imports
    DROP CONSTRAINT knowledge_imports_tenant_id_object_id_fkey,
    ADD COLUMN state TEXT NOT NULL DEFAULT 'complete' CHECK (state IN ('reserved','complete'));
