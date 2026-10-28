-- 027_assistant_audiences (OA-24, D-07): assistants carry a stored audience —
-- actor IDs or "*" tenant-wide — enforced ON TOP of stored resource grants,
-- never instead of them (D-25). Default '[]' means owner+platform.admin only;
-- owners opt members in by writing audience entries.
ALTER TABLE chatbots
    ADD COLUMN IF NOT EXISTS audience JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(audience) = 'array');

-- Member-list scans: chatbotAllowed checks `audience @> '["<actor>"]'`.
CREATE INDEX IF NOT EXISTS idx_chatbots_audience ON chatbots USING gin (audience);

-- Own-conversation keyset listing (OA-13): newest-first per owner with
-- deterministic (created_at, id) tie-breaking.
CREATE INDEX IF NOT EXISTS idx_conversations_owner_recent
    ON conversations (tenant_id, owner_actor_id, created_at DESC, id DESC);
