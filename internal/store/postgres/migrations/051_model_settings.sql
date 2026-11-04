-- Tenant policy per chat model alias, managed from the Models page: whether
-- people may use it, the tenant default model and the model a request falls
-- back to when this one has no working route. A missing row means available,
-- not default, no fallback.
CREATE TABLE model_settings (
    tenant_id      UUID NOT NULL REFERENCES tenants(id),
    model          TEXT NOT NULL,
    available      BOOLEAN NOT NULL DEFAULT true,
    is_default     BOOLEAN NOT NULL DEFAULT false,
    fallback_model TEXT NULL,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, model),
    CHECK (fallback_model IS NULL OR fallback_model <> model)
);

CREATE UNIQUE INDEX model_settings_one_default ON model_settings (tenant_id) WHERE is_default;
