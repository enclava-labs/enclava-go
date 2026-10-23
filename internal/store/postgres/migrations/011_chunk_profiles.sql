ALTER TABLE chunk_profiles
    ADD COLUMN preserve_clauses BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN preserve_authorship BOOLEAN NOT NULL DEFAULT false,
    ADD CONSTRAINT chunk_profiles_overlap_below_size
        CHECK (overlap_codepoints < max_size_codepoints) NOT VALID,
    ADD CONSTRAINT chunk_profiles_boundary_strategy
        CHECK (boundary_strategy IN ('fixed', 'heading', 'clause', 'paragraph', 'message', 'thread', 'code_lines')) NOT VALID,
    ADD CONSTRAINT chunk_profiles_table_handling
        CHECK (table_handling IN ('atomic', 'row-wise', 'skip')) NOT VALID;

CREATE FUNCTION reject_chunk_profile_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'chunk profile versions are immutable';
END;
$$;

CREATE TRIGGER chunk_profiles_immutable
    BEFORE UPDATE OR DELETE ON chunk_profiles
    FOR EACH ROW EXECUTE FUNCTION reject_chunk_profile_mutation();
