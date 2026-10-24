CREATE TABLE mcp_servers (
    tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    id TEXT NOT NULL,
    base_origin TEXT NOT NULL,
    endpoint_path TEXT NOT NULL DEFAULT '/',
    credential_ref TEXT NOT NULL DEFAULT '',
    authorized_private BOOLEAN NOT NULL DEFAULT FALSE,
    audience_scopes JSONB NOT NULL CHECK (jsonb_typeof(audience_scopes) = 'array'),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    CHECK (base_origin ~ '^https://[^/@?#]+$' OR (authorized_private AND base_origin ~ '^http://(localhost|127[.]0[.]0[.]1|[[]::1[]])(:[0-9]+)?$'))
);

CREATE TABLE mcp_tool_approvals (
    tenant_id UUID NOT NULL,
    server_id TEXT NOT NULL,
    tool_id TEXT NOT NULL,
    model_name TEXT NOT NULL,
    remote_name TEXT NOT NULL,
    input_schema JSONB NOT NULL,
    output_schema JSONB NOT NULL,
    input_schema_hash TEXT NOT NULL CHECK (input_schema_hash ~ '^[0-9a-f]{64}$'),
    output_schema_hash TEXT NOT NULL CHECK (output_schema_hash ~ '^[0-9a-f]{64}$'),
    schema_version BIGINT NOT NULL CHECK (schema_version > 0),
    revision BIGINT NOT NULL CHECK (revision > 0),
    caller_scopes JSONB NOT NULL CHECK (jsonb_typeof(caller_scopes) = 'array'),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, server_id, tool_id),
    UNIQUE (tenant_id, model_name),
    FOREIGN KEY (tenant_id, server_id) REFERENCES mcp_servers(tenant_id, id) ON DELETE CASCADE
);
CREATE TABLE mcp_tool_approval_history (
    tenant_id UUID NOT NULL,
    server_id TEXT NOT NULL,
    tool_id TEXT NOT NULL,
    revision BIGINT NOT NULL,
    remote_name TEXT NOT NULL,
    input_schema JSONB NOT NULL,
    output_schema JSONB NOT NULL,
    input_schema_hash TEXT NOT NULL,
    output_schema_hash TEXT NOT NULL,
    schema_version BIGINT NOT NULL,
    caller_scopes JSONB NOT NULL,
    enabled BOOLEAN NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, server_id, tool_id, revision),
    FOREIGN KEY (tenant_id, server_id) REFERENCES mcp_servers(tenant_id, id) ON DELETE CASCADE
);
