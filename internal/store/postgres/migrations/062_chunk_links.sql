-- v1.2 source links: per-chunk deep links (ordered code-point offsets into
-- chunk text). Qdrant mirrors these in the point payload for query time.
ALTER TABLE chunks ADD COLUMN links JSONB NOT NULL DEFAULT '[]';
