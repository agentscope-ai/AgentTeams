-- 0004: knowledge_nodes - the derived index (drop/rebuild allowed,
-- never the source of truth).
CREATE TABLE knowledge_nodes (
  anchor_id BIGINT NOT NULL,
  node_id VARCHAR(512) NOT NULL,
  kind VARCHAR(32) NOT NULL,                -- profile|memory|source|digest|daily|handoff|experience|skill
  path VARCHAR(1024) NOT NULL,
  title VARCHAR(512), version BIGINT, meta JSON,
  updated_at DATETIME(6) NOT NULL,
  PRIMARY KEY (anchor_id, node_id),
  KEY idx_path (anchor_id, path)
);
