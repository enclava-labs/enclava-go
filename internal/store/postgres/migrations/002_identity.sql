-- Identity schema (AUTH-01/AUTH-03): tenants, users, credentials, sessions,
-- service accounts, API keys and delegations.
--
-- Conventions for this and every later migration:
--   * every tenant-owned table carries tenant_id UUID NOT NULL;
--   * primary keys are (tenant_id, id);
--   * every reference to another tenant-owned row is a COMPOSITE foreign key
--     (tenant_id, ref_id) REFERENCES parent(tenant_id, id), so a row can never
--     point at another tenant's record;
--   * TIMESTAMPTZ timestamps, BIGINT amounts/counts, JSONB config/allowlists
--     with NOT NULL DEFAULT '{}', TEXT opaque IDs in the existing "<prefix>_"
--     style.

CREATE TABLE tenants (
    id           UUID PRIMARY KEY,
    slug         TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE users (
    tenant_id      UUID NOT NULL REFERENCES tenants(id),
    id             TEXT NOT NULL,
    email          TEXT NOT NULL,
    display_name   TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'active',
    password_hash  TEXT NULL,
    password_set_at TIMESTAMPTZ NULL,
    last_login_at  TIMESTAMPTZ NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, email)
);

CREATE TABLE roles (
    tenant_id  UUID NOT NULL REFERENCES tenants(id),
    id         TEXT NOT NULL,
    name       TEXT NOT NULL,
    scopes     JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name)
);

CREATE TABLE user_roles (
    tenant_id UUID NOT NULL REFERENCES tenants(id),
    user_id   TEXT NOT NULL,
    role_id   TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, user_id, role_id),
    FOREIGN KEY (tenant_id, user_id) REFERENCES users(tenant_id, id),
    FOREIGN KEY (tenant_id, role_id) REFERENCES roles(tenant_id, id)
);

CREATE TABLE service_accounts (
    tenant_id  UUID NOT NULL REFERENCES tenants(id),
    id         TEXT NOT NULL,
    name       TEXT NOT NULL,
    scopes     JSONB NOT NULL DEFAULT '{}',
    status     TEXT NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

CREATE TABLE auth_identities (
    tenant_id         UUID NOT NULL REFERENCES tenants(id),
    id                TEXT NOT NULL,
    user_id           TEXT NOT NULL,
    issuer            TEXT NOT NULL,
    subject           TEXT NOT NULL,
    linked_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    linked_by_user_id TEXT NULL,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, issuer, subject),
    FOREIGN KEY (tenant_id, user_id) REFERENCES users(tenant_id, id),
    FOREIGN KEY (tenant_id, linked_by_user_id) REFERENCES users(tenant_id, id)
);

CREATE TABLE oidc_providers (
    tenant_id         UUID NOT NULL REFERENCES tenants(id),
    id                TEXT NOT NULL,
    name              TEXT NOT NULL,
    issuer            TEXT NOT NULL,
    client_id         TEXT NOT NULL,
    client_secret_ref TEXT NOT NULL,
    redirect_path     TEXT NOT NULL,
    enabled           BOOLEAN NOT NULL DEFAULT true,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

CREATE TABLE oidc_states (
    tenant_id     UUID NOT NULL REFERENCES tenants(id),
    id            TEXT NOT NULL,
    state_hash    TEXT NOT NULL UNIQUE,
    provider_id   TEXT NOT NULL,
    user_id       TEXT NULL,
    nonce         TEXT NOT NULL,
    pkce_verifier TEXT NOT NULL,
    return_to     TEXT NOT NULL DEFAULT '',
    expires_at    TIMESTAMPTZ NOT NULL,
    consumed_at   TIMESTAMPTZ NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, provider_id) REFERENCES oidc_providers(tenant_id, id),
    FOREIGN KEY (tenant_id, user_id) REFERENCES users(tenant_id, id)
);

-- Server-side sessions: the hash is the lookup key and revoked_at is the
-- authoritative revocation flag — never cookie-only.
CREATE TABLE sessions (
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    id              TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    token_hash      TEXT NOT NULL UNIQUE,
    csrf_token      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,
    idle_expires_at TIMESTAMPTZ NOT NULL,
    revoked_at      TIMESTAMPTZ NULL,
    revoke_reason   TEXT NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, user_id) REFERENCES users(tenant_id, id)
);
CREATE INDEX idx_sessions_token_hash ON sessions(token_hash);

CREATE TABLE api_keys (
    tenant_id                 UUID NOT NULL REFERENCES tenants(id),
    id                        TEXT NOT NULL,
    owner_user_id             TEXT NULL,
    service_account_id        TEXT NULL,
    name                      TEXT NOT NULL,
    key_prefix                TEXT NOT NULL,
    key_hash                  TEXT NOT NULL,
    key_hash_algo             TEXT NOT NULL,
    scopes                    JSONB NOT NULL DEFAULT '{}',
    allowed_models            JSONB NOT NULL DEFAULT '{}',
    allowed_extract_templates JSONB NOT NULL DEFAULT '{}',
    allowed_chatbots          JSONB NOT NULL DEFAULT '{}',
    allowed_agents            JSONB NOT NULL DEFAULT '{}',
    allowed_endpoints         JSONB NOT NULL DEFAULT '{}',
    allowed_ips               JSONB NOT NULL DEFAULT '{}',
    rate_limit_per_minute     BIGINT NULL,
    rate_limit_per_hour       BIGINT NULL,
    rate_limit_per_day        BIGINT NULL,
    expires_at                TIMESTAMPTZ NULL,
    revoked_at                TIMESTAMPTZ NULL,
    last_used_at              TIMESTAMPTZ NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, key_hash),
    -- A key always belongs to a user or a service account — never global.
    CHECK (owner_user_id IS NOT NULL OR service_account_id IS NOT NULL),
    FOREIGN KEY (tenant_id, owner_user_id) REFERENCES users(tenant_id, id),
    FOREIGN KEY (tenant_id, service_account_id) REFERENCES service_accounts(tenant_id, id)
);
CREATE INDEX idx_api_keys_tenant_prefix ON api_keys(tenant_id, key_prefix);

CREATE TABLE delegations (
    tenant_id                   UUID NOT NULL REFERENCES tenants(id),
    id                          TEXT NOT NULL,
    delegator_user_id           TEXT NOT NULL,
    delegate_service_account_id TEXT NOT NULL,
    allowed_scopes              JSONB NOT NULL DEFAULT '{}',
    allowed_collections         JSONB NOT NULL DEFAULT '{}',
    expires_at                  TIMESTAMPTZ NOT NULL,
    revoked_at                  TIMESTAMPTZ NULL,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, delegator_user_id) REFERENCES users(tenant_id, id),
    FOREIGN KEY (tenant_id, delegate_service_account_id) REFERENCES service_accounts(tenant_id, id)
);
