CREATE TABLE security_audit (
    tenant_id UUID NOT NULL REFERENCES tenants(id),
    id TEXT NOT NULL,
    actor_id TEXT NOT NULL DEFAULT '',
    actor_kind TEXT NOT NULL DEFAULT '',
    credential_id TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL,
    target_kind TEXT NOT NULL DEFAULT '',
    target_id TEXT NOT NULL DEFAULT '',
    outcome TEXT NOT NULL DEFAULT '',
    client_ip TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    occurred_at TIMESTAMPTZ NOT NULL,
    details JSONB NOT NULL DEFAULT '{}',
    PRIMARY KEY (tenant_id, id)
);

CREATE INDEX security_audit_tenant_order_idx
    ON security_audit (tenant_id, occurred_at DESC, id DESC);

CREATE FUNCTION reject_security_audit_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'security audit records are append-only';
END;
$$;

CREATE TRIGGER security_audit_no_update_delete
    BEFORE UPDATE OR DELETE ON security_audit
    FOR EACH ROW EXECUTE FUNCTION reject_security_audit_mutation();

CREATE TRIGGER security_audit_no_truncate
    BEFORE TRUNCATE ON security_audit
    FOR EACH STATEMENT EXECUTE FUNCTION reject_security_audit_mutation();
