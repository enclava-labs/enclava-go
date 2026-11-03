-- Member "Request API access" records (P-12/UX-M03): a request asks an
-- administrator for an API key — nothing is granted automatically. The
-- partial unique index enforces at most one OPEN request per user; a
-- dismissed request leaves the audit row and frees the slot.
CREATE TABLE api_access_requests (
    tenant_id    UUID NOT NULL REFERENCES tenants(id),
    id           TEXT NOT NULL,
    user_id      TEXT NOT NULL,
    note         TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','dismissed')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    dismissed_at TIMESTAMPTZ NULL,
    dismissed_by TEXT NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, user_id) REFERENCES users(tenant_id, id)
);

CREATE UNIQUE INDEX api_access_requests_one_open
    ON api_access_requests (tenant_id, user_id)
    WHERE status = 'open';
