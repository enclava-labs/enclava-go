-- Each generation records the index it was embedded into, so a collection
-- can switch embedding model or vector size while its documents re-embed in
-- the background: search reads every published generation from its own
-- index with its own model. Indexes a collection switched away from are
-- listed until no live generation reads them, then dropped.
ALTER TABLE index_generations
    ADD COLUMN qdrant_collection    TEXT    NOT NULL DEFAULT '',
    ADD COLUMN embedding_model      TEXT    NOT NULL DEFAULT '',
    ADD COLUMN embedding_dimensions INTEGER NOT NULL DEFAULT 0;

UPDATE index_generations g
   SET qdrant_collection    = c.qdrant_collection,
       embedding_model      = c.embedding_model,
       embedding_dimensions = c.embedding_dimensions
  FROM knowledge_collections c
 WHERE c.tenant_id = g.tenant_id AND c.id = g.collection_id;

ALTER TABLE knowledge_collections
    ADD COLUMN retired_qdrant_collections JSONB NOT NULL DEFAULT '[]';
