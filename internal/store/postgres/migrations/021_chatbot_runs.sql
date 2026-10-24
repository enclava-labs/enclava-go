ALTER TABLE messages
    ADD COLUMN evidence_ids JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(evidence_ids) = 'array'),
    ADD COLUMN input_refs JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(input_refs) = 'array');

CREATE TABLE chatbot_runs (
    tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    id TEXT NOT NULL,
    chatbot_id TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    job_id TEXT NOT NULL DEFAULT '',
    owner_actor_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('queued','running','succeeded','failed','cancelled','unknown')),
    fence_token BIGINT NOT NULL DEFAULT 0,
    input TEXT NOT NULL,
    output TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    config_snapshot JSONB NOT NULL,
    prompt_template_id TEXT NOT NULL DEFAULT '',
    prompt_template_version INTEGER NOT NULL DEFAULT 0,
    evidence_ids JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(evidence_ids) = 'array'),
    input_refs JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(input_refs) = 'array'),
    restrictions JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(restrictions) = 'array'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, chatbot_id) REFERENCES chatbots(tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, conversation_id) REFERENCES conversations(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX chatbot_runs_owner_recent ON chatbot_runs(tenant_id, owner_actor_id, created_at DESC);

CREATE TABLE chatbot_run_attempts (
    tenant_id UUID NOT NULL,
    run_id TEXT NOT NULL,
    number INTEGER NOT NULL CHECK (number > 0),
    kind TEXT NOT NULL CHECK (kind IN ('model','tool')),
    name TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('running','succeeded','failed','cancelled','unknown')),
    input TEXT NOT NULL DEFAULT '',
    output TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, run_id, number),
    FOREIGN KEY (tenant_id, run_id) REFERENCES chatbot_runs(tenant_id, id) ON DELETE CASCADE
);
