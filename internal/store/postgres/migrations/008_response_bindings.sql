-- Response/item bindings (INF-04/INF-06): every public Responses reference is
-- a server-minted opaque ID bound to tenant, owner, endpoint origin, the raw
-- upstream identifier, its parent response and the immutable input snapshot it
-- was created under. Possession of a raw upstream ID is never authority; a raw
-- upstream ID shared across two endpoints binds to distinct local resources.
-- Items scope their upstream collision domain under the parent response via
-- (tenant, endpoint, parent, upstream); responses carry parent ''.

CREATE TABLE response_bindings (
    tenant_id        UUID        NOT NULL REFERENCES tenants(id),
    public_id        TEXT        NOT NULL,
    kind             TEXT        NOT NULL CHECK (kind IN ('response', 'item')),
    endpoint_id      TEXT        NOT NULL,
    owner_actor_id   TEXT        NOT NULL,
    delegated_by     TEXT        NOT NULL DEFAULT '',
    upstream_id      TEXT        NOT NULL,
    parent_public_id TEXT        NOT NULL DEFAULT '',
    input_snapshot   JSONB       NOT NULL DEFAULT '{}'::jsonb,
    state            TEXT        NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'deleted')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, public_id),
    UNIQUE (tenant_id, endpoint_id, parent_public_id, upstream_id)
);

CREATE INDEX response_bindings_owner_idx
    ON response_bindings (tenant_id, owner_actor_id, kind, state);

CREATE INDEX response_bindings_parent_idx
    ON response_bindings (tenant_id, parent_public_id);
