ALTER TABLE users ADD COLUMN deleted_at TIMESTAMPTZ;
ALTER TABLE users DROP CONSTRAINT users_tenant_id_email_key;
CREATE UNIQUE INDEX users_active_tenant_email_key ON users (tenant_id, email) WHERE deleted_at IS NULL;
