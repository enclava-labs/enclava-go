-- Tenant-scoped inference endpoint records (INF-01 storage half). Replaces
-- the legacy providers concept entirely — an endpoint binds protocol,
-- capabilities, base origin, credential reference and real assurance state.
-- assurance_verified_at records an actual attested verification timestamp;
-- authorized_private is a separate explicit administrative flag — neither is
-- inferred from the other.

CREATE TABLE inference_endpoints (
    tenant_id            UUID NOT NULL REFERENCES tenants(id),
    id                   TEXT NOT NULL,
    protocol             TEXT NOT NULL,
    base_origin          TEXT NOT NULL,
    credential_ref       TEXT NOT NULL DEFAULT '',
    capabilities         JSONB NOT NULL DEFAULT '[]',
    assurance_mode       TEXT NOT NULL DEFAULT '',
    assurance_verified_at TIMESTAMPTZ NULL,
    authorized_private   BOOLEAN NOT NULL DEFAULT false,
    enabled              BOOLEAN NOT NULL DEFAULT true,
    config               JSONB NOT NULL DEFAULT '{}',
    created_by           TEXT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX idx_inference_endpoints_tenant ON inference_endpoints (tenant_id) WHERE enabled;
