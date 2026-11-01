-- Prefix each chunk with its document's name before embedding. New
-- collections turn it on; existing ones were embedded without it and keep
-- that until changed (which re-embeds them).
ALTER TABLE knowledge_collections
    ADD COLUMN embed_titles BOOLEAN NOT NULL DEFAULT false;
