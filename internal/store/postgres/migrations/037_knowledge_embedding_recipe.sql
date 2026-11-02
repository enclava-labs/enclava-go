ALTER TABLE knowledge_collections
    ADD COLUMN query_prefix TEXT NOT NULL DEFAULT '',
    ADD COLUMN passage_prefix TEXT NOT NULL DEFAULT '',
    ADD COLUMN retired_index_since JSONB NOT NULL DEFAULT '{}';

ALTER TABLE index_generations
    ADD COLUMN query_prefix TEXT NOT NULL DEFAULT '',
    ADD COLUMN passage_prefix TEXT NOT NULL DEFAULT '',
    ADD COLUMN warnings JSONB NOT NULL DEFAULT '[]';

UPDATE knowledge_collections
SET query_prefix = E'Instruct: Given a web search query, retrieve relevant passages that answer the query\nQuery: '
WHERE lower(embedding_model) LIKE '%qwen3-embedding%';

UPDATE index_generations g
SET query_prefix = E'Instruct: Given a web search query, retrieve relevant passages that answer the query\nQuery: '
FROM knowledge_collections c
WHERE c.tenant_id = g.tenant_id AND c.id = g.collection_id
  AND lower(COALESCE(NULLIF(g.embedding_model, ''), c.embedding_model)) LIKE '%qwen3-embedding%';

UPDATE knowledge_collections c
SET retired_index_since = COALESCE((
    SELECT jsonb_object_agg(name, now())
    FROM jsonb_array_elements_text(c.retired_qdrant_collections) AS names(name)
), '{}'::jsonb);
