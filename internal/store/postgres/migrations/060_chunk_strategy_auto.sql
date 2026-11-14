-- "auto" picks the boundary strategy per document from the parsed blocks.
ALTER TABLE chunk_profiles DROP CONSTRAINT chunk_profiles_boundary_strategy;
ALTER TABLE chunk_profiles
    ADD CONSTRAINT chunk_profiles_boundary_strategy
        CHECK (boundary_strategy IN ('auto', 'fixed', 'heading', 'clause', 'paragraph', 'message', 'thread', 'code_lines')) NOT VALID;
