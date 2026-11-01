-- A collection may index a shortened (Matryoshka) prefix of its model's
-- embedding: 0 keeps the model's full output; otherwise document and query
-- vectors are cut to this many dimensions and re-normalized.
ALTER TABLE knowledge_collections
    ADD COLUMN embedding_dimensions INTEGER NOT NULL DEFAULT 0
    CHECK (embedding_dimensions >= 0);
