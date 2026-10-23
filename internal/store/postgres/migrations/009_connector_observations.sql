CREATE TABLE connector_streams (
    tenant_id UUID NOT NULL,
    connection_id TEXT NOT NULL,
    stream TEXT NOT NULL CHECK (stream IN ('content', 'access', 'membership')),
    run_id TEXT NOT NULL,
    fence BIGINT NOT NULL,
    cursor TEXT NOT NULL DEFAULT '',
    observed_seq BIGINT NOT NULL DEFAULT 0,
    run_start_seq BIGINT NOT NULL DEFAULT 0,
    complete BOOLEAN NOT NULL DEFAULT FALSE,
    incomplete BOOLEAN NOT NULL DEFAULT FALSE,
    last_error TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, connection_id, stream),
    FOREIGN KEY (tenant_id, connection_id) REFERENCES connections(tenant_id, id) ON DELETE CASCADE
);

CREATE TABLE connector_observations (
    tenant_id UUID NOT NULL,
    connection_id TEXT NOT NULL,
    stream TEXT NOT NULL CHECK (stream IN ('content', 'access', 'membership')),
    run_id TEXT NOT NULL,
    observed_seq BIGINT NOT NULL,
    native_id TEXT NOT NULL,
    object_id TEXT,
    source_observed_seq BIGINT NOT NULL DEFAULT 0,
    payload JSONB NOT NULL,
    artifact_ref JSONB,
    processing_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, connection_id, stream, observed_seq),
    FOREIGN KEY (tenant_id, connection_id) REFERENCES connections(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX idx_connector_observations_native ON connector_observations(tenant_id, connection_id, stream, native_id, observed_seq DESC);
CREATE INDEX idx_connector_observations_pending ON connector_observations(tenant_id, stream, created_at) WHERE processing_error = '';
