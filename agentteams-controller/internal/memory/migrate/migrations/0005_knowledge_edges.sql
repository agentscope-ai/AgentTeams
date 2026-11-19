-- 0005: knowledge_edges - the derived index (drop/rebuild allowed).
CREATE TABLE knowledge_edges (
  anchor_id BIGINT NOT NULL,
  src_id VARCHAR(512) NOT NULL,
  dst_id VARCHAR(512) NOT NULL,
  kind VARCHAR(32) NOT NULL,                -- wikilink|source|promotion|reference
  PRIMARY KEY (anchor_id, src_id, dst_id, kind)
);
