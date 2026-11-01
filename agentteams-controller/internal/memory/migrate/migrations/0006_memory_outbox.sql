-- 0006: memory_outbox - event log written in the same transaction as
-- the business write; readers cursor on seq, published_at IS NULL
-- means pending.
CREATE TABLE memory_outbox (
  seq BIGINT AUTO_INCREMENT PRIMARY KEY,
  aggregate_type VARCHAR(32) NOT NULL,      -- entry|anchor|promotion|export
  aggregate_id VARCHAR(512) NOT NULL,
  anchor_id BIGINT NOT NULL,
  event_type VARCHAR(64) NOT NULL,          -- memory.entry.created|updated|deleted|promoted
  payload JSON NOT NULL,
  created_at DATETIME(6) NOT NULL,
  published_at DATETIME(6),
  attempts INT NOT NULL DEFAULT 0,
  last_error TEXT,
  KEY idx_outbox_pending (published_at, seq)
);
