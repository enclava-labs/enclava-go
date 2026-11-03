-- Optional per-template generation cap (EXT-REAL-I01): NULL/0 means no
-- explicit cap — the executor injects its configured default and clamps to
-- the routed model's catalog limits. 1..32768 is the validated surface range.
ALTER TABLE extract_templates
    ADD COLUMN max_output_tokens INTEGER NOT NULL DEFAULT 0
        CHECK (max_output_tokens >= 0 AND max_output_tokens <= 32768);
