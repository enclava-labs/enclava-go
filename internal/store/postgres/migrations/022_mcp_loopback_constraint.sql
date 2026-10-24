ALTER TABLE mcp_servers DROP CONSTRAINT mcp_servers_check;
ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_check CHECK (
    base_origin ~ '^https://[^/@?#]+$' OR
    (authorized_private AND base_origin ~ '^http://(localhost|127[.]0[.]0[.]1|\\[::1\\])(:[0-9]+)?$')
);
