-- Deleting things over the API must succeed for used objects, not only
-- empty ones. Three delete paths were permanently broken by RESTRICT
-- foreign keys: chatbots with conversations (DELETE /api/v1/chatbots),
-- conversations with messages, and knowledge collections with documents
-- (DELETE /api/v1/rag/collections — the service already wipes the vector
-- store first, so the relational rows must follow). chatbot_runs already
-- cascades (021) and is left untouched.
DO $$
DECLARE cname text;
BEGIN
    SELECT con.conname INTO cname FROM pg_constraint con
    WHERE con.conrelid = 'conversations'::regclass AND con.confrelid = 'chatbots'::regclass;
    EXECUTE format('ALTER TABLE conversations DROP CONSTRAINT %I', cname);
    SELECT con.conname INTO cname FROM pg_constraint con
    WHERE con.conrelid = 'messages'::regclass AND con.confrelid = 'conversations'::regclass;
    EXECUTE format('ALTER TABLE messages DROP CONSTRAINT %I', cname);
    SELECT con.conname INTO cname FROM pg_constraint con
    WHERE con.conrelid = 'collection_items'::regclass AND con.confrelid = 'knowledge_collections'::regclass;
    EXECUTE format('ALTER TABLE collection_items DROP CONSTRAINT %I', cname);
    SELECT con.conname INTO cname FROM pg_constraint con
    WHERE con.conrelid = 'index_generations'::regclass AND con.confrelid = 'knowledge_collections'::regclass;
    EXECUTE format('ALTER TABLE index_generations DROP CONSTRAINT %I', cname);
    SELECT con.conname INTO cname FROM pg_constraint con
    WHERE con.conrelid = 'knowledge_imports'::regclass AND con.confrelid = 'knowledge_collections'::regclass;
    EXECUTE format('ALTER TABLE knowledge_imports DROP CONSTRAINT %I', cname);
    SELECT con.conname INTO cname FROM pg_constraint con
    WHERE con.conrelid = 'rag_evidence'::regclass AND con.confrelid = 'knowledge_collections'::regclass;
    EXECUTE format('ALTER TABLE rag_evidence DROP CONSTRAINT %I', cname);
    SELECT con.conname INTO cname FROM pg_constraint con
    WHERE con.conrelid = 'index_reconciliation'::regclass AND con.confrelid = 'index_generations'::regclass;
    EXECUTE format('ALTER TABLE index_reconciliation DROP CONSTRAINT %I', cname);
END $$;

ALTER TABLE conversations
    ADD CONSTRAINT conversations_chatbot_fk
    FOREIGN KEY (tenant_id, chatbot_id)
    REFERENCES chatbots(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE messages
    ADD CONSTRAINT messages_conversation_fk
    FOREIGN KEY (tenant_id, conversation_id)
    REFERENCES conversations(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE collection_items
    ADD CONSTRAINT collection_items_collection_fk
    FOREIGN KEY (tenant_id, collection_id)
    REFERENCES knowledge_collections(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE index_generations
    ADD CONSTRAINT index_generations_collection_fk
    FOREIGN KEY (tenant_id, collection_id)
    REFERENCES knowledge_collections(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE knowledge_imports
    ADD CONSTRAINT knowledge_imports_collection_fk
    FOREIGN KEY (tenant_id, collection_id)
    REFERENCES knowledge_collections(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE rag_evidence
    ADD CONSTRAINT rag_evidence_collection_fk
    FOREIGN KEY (tenant_id, collection_id)
    REFERENCES knowledge_collections(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE index_reconciliation
    ADD CONSTRAINT index_reconciliation_generation_fk
    FOREIGN KEY (tenant_id, collection_id, object_version_id, generation)
    REFERENCES index_generations(tenant_id, collection_id, object_version_id, generation)
    ON DELETE CASCADE;
