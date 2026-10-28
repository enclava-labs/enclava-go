-- 026_mcp_discovery (OA-26 bridge, OA-19 attention source): durable MCP tool
-- discovery rows. Tenant/server identity lives on the row — never derived
-- from a snapshot's tools array — so an empty discovery revokes exactly its
-- server's approvals. schema_version is the durable CURRENT version: it is
-- initialized to 1 on first discovery, unchanged while the canonical digests
-- stay equal, and incremented exactly when a rediscovery's digests differ; it
-- survives restarts and backs the atomic approve/reject review protocol.
-- input_digest/output_digest are mcp.SchemaDigest values: canonical-JSON
-- SHA-256, stable across jsonb round-trips.
CREATE TABLE mcp_tool_discoveries (
    tenant_id UUID NOT NULL,
    server_id TEXT NOT NULL,
    tool_id TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    annotations JSONB NOT NULL DEFAULT '{}'::jsonb,
    remote_name TEXT NOT NULL DEFAULT '',
    input_schema JSONB NOT NULL,
    output_schema JSONB NOT NULL,
    input_digest TEXT NOT NULL CHECK (input_digest ~ '^[0-9a-f]{64}$'),
    output_digest TEXT NOT NULL CHECK (output_digest ~ '^[0-9a-f]{64}$'),
    schema_version BIGINT NOT NULL DEFAULT 1 CHECK (schema_version > 0),
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','changed','approved','rejected','gone')),
    approved_schema_version BIGINT NULL CHECK (approved_schema_version > 0),
    prior_input_schema JSONB NULL,
    prior_output_schema JSONB NULL,
    diff_fields JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(diff_fields) = 'array'),
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, server_id, tool_id),
    FOREIGN KEY (tenant_id, server_id) REFERENCES mcp_servers(tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX idx_mcp_tool_discoveries_tenant_state ON mcp_tool_discoveries (tenant_id, state);
