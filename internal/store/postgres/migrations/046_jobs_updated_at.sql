ALTER TABLE jobs ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
UPDATE jobs SET updated_at = COALESCE(finished_at, started_at, submitted_at);
