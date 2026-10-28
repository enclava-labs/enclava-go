ALTER TABLE messages
    ADD COLUMN run_id TEXT NULL;

CREATE INDEX idx_messages_conversation_run ON messages (tenant_id, conversation_id, run_id);

ALTER TABLE chatbot_runs
    ADD COLUMN endpoint_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN model TEXT NOT NULL DEFAULT '';
