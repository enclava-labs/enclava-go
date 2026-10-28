-- 029_identity_admin (OA-20/OA-28/OA-29, plan 08-02): users carry a free-form
-- team label — the §6.12 spend-by-team join key and the Access › People team
-- column (empty = "No team", a first-class state). Roles gain an updated_at
-- version stamp so UpdateRoleAtomically can refuse a handler's pre-lock read
-- that a concurrent write has overtaken (409 stale_role), instead of merging
-- deltas onto a superseded row.
ALTER TABLE users ADD COLUMN IF NOT EXISTS team TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_users_tenant_team ON users (tenant_id, team);

ALTER TABLE roles ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
