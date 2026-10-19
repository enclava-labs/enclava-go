-- Exact billing schema (JOB-03/JOB-04): prices, budgets, reservations and
-- usage records. Money is integer currency-micros and unit counts are
-- integer — no REAL/DOUBLE/FLOAT column exists anywhere in this file.

-- One row per unit kind, each an exact rational rate:
-- rate_micros currency-micros per unit_quantity of unit_kind. unit_kind is
-- input|output|cached_input or a named advertised unit.
CREATE TABLE billing_prices (
    tenant_id      UUID NOT NULL REFERENCES tenants(id),
    id             TEXT NOT NULL,
    subject_type   TEXT NOT NULL CHECK (subject_type IN ('endpoint','model')),
    subject_id     TEXT NOT NULL,
    capability     TEXT NOT NULL,
    unit_kind      TEXT NOT NULL,
    rate_micros    BIGINT NOT NULL CHECK (rate_micros >= 0),
    unit_quantity  BIGINT NOT NULL CHECK (unit_quantity > 0),
    currency       CHAR(3) NOT NULL,
    effective_from TIMESTAMPTZ NOT NULL,
    created_by     TEXT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, subject_type, subject_id, capability, unit_kind, effective_from)
);

CREATE TABLE billing_price_audit (
    tenant_id  UUID NOT NULL REFERENCES tenants(id),
    id         TEXT NOT NULL,
    price_id   TEXT NOT NULL,
    action     TEXT NOT NULL,
    actor_type TEXT NOT NULL,
    actor_id   TEXT NOT NULL,
    detail     JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, price_id) REFERENCES billing_prices(tenant_id, id)
);

-- Amount budgets carry their bound currency — a reservation priced in
-- another currency is denied, no FX. Token budgets have NULL currency.
CREATE TABLE budgets (
    tenant_id    UUID NOT NULL REFERENCES tenants(id),
    id           TEXT NOT NULL,
    name         TEXT NOT NULL,
    subject_type TEXT NOT NULL CHECK (subject_type IN ('tenant','api_key','user','service_account')),
    subject_id   TEXT NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('amount','tokens')),
    limit_value  BIGINT NOT NULL CHECK (limit_value >= 0),
    currency     CHAR(3) NULL,
    period       TEXT NOT NULL CHECK (period IN ('daily','weekly','monthly')),
    active       BOOLEAN NOT NULL DEFAULT true,
    starts_at    TIMESTAMPTZ NOT NULL,
    ends_at      TIMESTAMPTZ NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    CHECK (kind = 'tokens' OR currency IS NOT NULL)
);
CREATE INDEX idx_budgets_subject ON budgets (tenant_id, subject_type, subject_id);

-- An atomic pre-attempt hold. price_snapshot is the immutable
-- {rates:[{unit,rate_micros,unit_quantity}],currency,rounding_rule} JSON used
-- verbatim at settle; reserved_units is the persisted per-kind reserved
-- quantity map {unit_kind: bounded_units} so held/unknown token-budget
-- accounting reconstructs liability across processes, including zero-rate
-- units. currency is the single bound currency of this reservation —
-- amount-budget comparison only sums same-currency liabilities.
CREATE TABLE reservations (
    tenant_id            UUID NOT NULL REFERENCES tenants(id),
    id                   TEXT NOT NULL,
    job_id               TEXT NULL,
    attempt_id           TEXT NULL,
    subject_type         TEXT NOT NULL,
    subject_id           TEXT NOT NULL,
    price_id             TEXT NULL,
    price_snapshot       JSONB NOT NULL,
    reserved_units       JSONB NOT NULL,
    limit_amount_micros  BIGINT NOT NULL CHECK (limit_amount_micros >= 0),
    currency             CHAR(3) NOT NULL,
    state                TEXT NOT NULL CHECK (state IN ('held','settled','unknown','released')),
    dispatched_at        TIMESTAMPTZ NULL,
    settled_at           TIMESTAMPTZ NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, job_id) REFERENCES jobs(tenant_id, id),
    FOREIGN KEY (tenant_id, attempt_id) REFERENCES job_attempts(tenant_id, id),
    FOREIGN KEY (tenant_id, price_id) REFERENCES billing_prices(tenant_id, id)
);
CREATE INDEX idx_reservations_subject ON reservations (tenant_id, subject_type, subject_id);
CREATE INDEX idx_reservations_state ON reservations (state);

-- Insert-only settlement evidence: a reservation settles exactly once.
-- units is the raw reported per-kind usage {unit_kind: raw_units} — never
-- clipped; overruns are preserved. attempt_identity deduplicates a retry of
-- the same upstream attempt.
CREATE TABLE usage_records (
    tenant_id        UUID NOT NULL REFERENCES tenants(id),
    id               TEXT NOT NULL,
    reservation_id   TEXT NOT NULL,
    subject_type     TEXT NOT NULL,
    subject_id       TEXT NOT NULL,
    capability       TEXT NOT NULL,
    model            TEXT NOT NULL,
    endpoint_id      TEXT NULL,
    units            JSONB NOT NULL,
    amount_micros    BIGINT NOT NULL,
    currency         CHAR(3) NOT NULL,
    price_snapshot   JSONB NOT NULL,
    outcome          TEXT NOT NULL CHECK (outcome IN ('completed','unknown','not_dispatched')),
    attempt_identity TEXT NULL UNIQUE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, reservation_id),
    FOREIGN KEY (tenant_id, reservation_id) REFERENCES reservations(tenant_id, id)
);

CREATE TABLE billing_audit (
    tenant_id    UUID NOT NULL REFERENCES tenants(id),
    id           TEXT NOT NULL,
    subject_type TEXT NOT NULL,
    subject_id   TEXT NOT NULL,
    action       TEXT NOT NULL,
    actor_type   TEXT NOT NULL,
    actor_id     TEXT NOT NULL,
    detail       JSONB NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);
