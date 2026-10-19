ALTER TABLE source_restrictions DROP CONSTRAINT source_restrictions_subject_type_check;
ALTER TABLE source_restrictions ADD CONSTRAINT source_restrictions_subject_type_check
    CHECK (subject_type IN ('tenant','connection','object','artifact'));
