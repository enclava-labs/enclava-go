-- OIDC identity-linking extension (AUTH-02): an oidc_states row can also
-- carry a pending interactive link decision, so the verified issuer+subject
-- survives the callback→confirmation round trip server-side instead of being
-- re-asserted by the browser.

ALTER TABLE oidc_states
    ADD COLUMN subject TEXT NOT NULL DEFAULT '',
    ADD COLUMN email   TEXT NOT NULL DEFAULT '',
    ADD COLUMN pending_link_user_id TEXT NULL;

ALTER TABLE oidc_states
    ADD CONSTRAINT oidc_states_pending_link_user_fk
    FOREIGN KEY (tenant_id, pending_link_user_id)
    REFERENCES users(tenant_id, id);
