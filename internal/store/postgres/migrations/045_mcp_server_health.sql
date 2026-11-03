-- 045_mcp_server_health (TOOLS-I04): persist the outcome of the last tool
-- discovery on the server row itself. last_discovery_at NULL means the server
-- was never probed; a non-empty last_discovery_error means the last probe
-- failed (the UI renders Connected/Unreachable from this pair, never "enabled
-- implies connected").
ALTER TABLE mcp_servers
    ADD COLUMN last_discovery_at TIMESTAMPTZ NULL,
    ADD COLUMN last_discovery_error TEXT NOT NULL DEFAULT '';
