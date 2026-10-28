-- 028_usage_presentation (OA-19/OA-24/OA-27/OA-29, plan 08-07): verified
-- credential attribution plus the covering indexes the usage screens read.
-- credential_id carries the api-key credential a reservation was admitted
-- for ('' = session or other credential kinds) so key-scoped budgets and
-- per-key spend reads resolve real settled rows while the canonical billing
-- subject stays the owner — usage is never double-counted.
ALTER TABLE reservations ADD COLUMN credential_id TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_records ADD COLUMN credential_id TEXT NOT NULL DEFAULT '';

-- Daily-series and by-team/by-subject group-bys scan settled usage inside a
-- tenant window; per-key liability reads filter by credential.
CREATE INDEX idx_usage_records_tenant_created
    ON usage_records (tenant_id, created_at);
CREATE INDEX idx_usage_records_subject_created
    ON usage_records (tenant_id, subject_type, subject_id, created_at);
CREATE INDEX idx_usage_records_credential_created
    ON usage_records (tenant_id, credential_id, created_at);
