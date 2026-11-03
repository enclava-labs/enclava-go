-- Track the last configuration change on connector connections so source
-- views can report created_at/updated_at honestly.
ALTER TABLE connections
    ADD COLUMN updated_at TIMESTAMPTZ;
UPDATE connections SET updated_at = created_at;
ALTER TABLE connections ALTER COLUMN updated_at SET DEFAULT now();
ALTER TABLE connections ALTER COLUMN updated_at SET NOT NULL;
