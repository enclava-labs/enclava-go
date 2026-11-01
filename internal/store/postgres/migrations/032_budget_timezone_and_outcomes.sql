-- Budget liability windows gain an IANA timezone ("" defaults to UTC):
-- day/ISO-week/month period starts are computed in that zone. usage_records
-- outcomes grow by 'unpriced_units' (settlement recorded units whose kinds
-- had no snapshot rate) and 'reconciled' (admin resolution of an unknown
-- reservation).

ALTER TABLE budgets ADD COLUMN timezone TEXT NOT NULL DEFAULT 'UTC';

ALTER TABLE usage_records DROP CONSTRAINT usage_records_outcome_check;
ALTER TABLE usage_records ADD CONSTRAINT usage_records_outcome_check
    CHECK (outcome IN ('completed','unknown','not_dispatched','unpriced_units','reconciled'));
