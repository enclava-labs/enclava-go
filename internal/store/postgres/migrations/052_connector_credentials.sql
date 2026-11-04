-- Connector credentials administrators manage in the dashboard: pasted API
-- tokens, tokens obtained through a provider's OAuth flow, and each
-- provider's OAuth app (client id + secret). Secret material is sealed with
-- AES-256-GCM under ENCLAVA_SECRETS_KEY, bound to (tenant, id); details hold
-- only non-secret facts (account, scopes, expiry, client id, API origin).
-- id doubles as the credential_ref sources use (CONNECTOR_<PROVIDER>_<NAME>).
CREATE TABLE connector_credentials (
    tenant_id  UUID NOT NULL REFERENCES tenants(id),
    id         TEXT NOT NULL,
    provider   TEXT NOT NULL CHECK (provider IN ('github','slack','notion','teams','google_drive')),
    kind       TEXT NOT NULL CHECK (kind IN ('token','oauth','oauth_app')),
    name       TEXT NOT NULL,
    sealed     BYTEA NOT NULL,
    details    JSONB NOT NULL DEFAULT '{}',
    created_by TEXT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

CREATE UNIQUE INDEX connector_credentials_name ON connector_credentials (tenant_id, provider, lower(name)) WHERE kind <> 'oauth_app';

-- Single-use OAuth authorization states, bound to the starting user.
CREATE TABLE connector_oauth_states (
    state      TEXT PRIMARY KEY,
    tenant_id  UUID NOT NULL REFERENCES tenants(id),
    user_id    TEXT NOT NULL,
    provider   TEXT NOT NULL,
    name       TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);
