ALTER TABLE access_snapshots
    ADD COLUMN unmapped_subjects INT NOT NULL DEFAULT 0;

ALTER TABLE connector_streams
    ADD COLUMN config_hash TEXT NOT NULL DEFAULT '';

ALTER TABLE source_grants DROP CONSTRAINT source_grants_subject_type_check;
ALTER TABLE source_grants ADD CONSTRAINT source_grants_subject_type_check
    CHECK (subject_type IN ('user', 'group', 'all'));
ALTER TABLE source_grants ADD CONSTRAINT source_grants_all_subject_check
    CHECK (subject_type <> 'all' OR (source_identity_id IS NULL AND source_group_id IS NULL));
