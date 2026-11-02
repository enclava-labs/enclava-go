-- A generation also records whether its chunks were embedded behind their
-- document name, so a collection can tell which published generations still
-- reflect old settings and resume re-embedding them after an interrupted
-- settings change.
ALTER TABLE index_generations
    ADD COLUMN embed_titles BOOLEAN NOT NULL DEFAULT false;

UPDATE index_generations g
   SET embed_titles = c.embed_titles
  FROM knowledge_collections c
 WHERE c.tenant_id = g.tenant_id AND c.id = g.collection_id AND g.qdrant_collection <> '';
