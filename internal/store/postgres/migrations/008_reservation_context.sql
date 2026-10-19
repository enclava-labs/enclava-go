-- Reservations carry their priced dispatch context so settle writes truthful
-- usage_records without trusting a caller-supplied model/capability at settle
-- time (usage_records.capability/model are NOT NULL). One reservation also
-- spans every subject key whose budgets were checked at admission: the
-- canonical row keeps the reservation id; additional subject-attribution rows
-- share it via a "<id>/<type>:<subject>" suffix so per-subject liability is
-- reconstructible while transitions address the whole group by prefix.

ALTER TABLE reservations
    ADD COLUMN capability  TEXT NOT NULL DEFAULT '',
    ADD COLUMN model       TEXT NOT NULL DEFAULT '',
    ADD COLUMN endpoint_id TEXT NULL;
