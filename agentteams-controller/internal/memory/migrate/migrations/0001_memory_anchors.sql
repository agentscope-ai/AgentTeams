-- 0001: memory_anchors - the anchor registry.
-- owner -> backend pointer only; entry content never lives here
-- (implementation plan 3.1).
CREATE TABLE memory_anchors (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  owner_type VARCHAR(16) NOT NULL,          -- human | team | agent
  owner_id VARCHAR(256) NOT NULL,
  backend_kind VARCHAR(32) NOT NULL,        -- powercontext | remelight-file | adbpg
  backend_location VARCHAR(512),
  capabilities JSON NOT NULL,
  backend_version VARCHAR(64),
  created_at DATETIME(6) NOT NULL, updated_at DATETIME(6) NOT NULL,
  UNIQUE KEY uq_anchor (owner_type, owner_id)
);
