-- ops.outbox（R1 provision 通道，与 controller 侧同构、独立 schema）冻结 DDL。
CREATE TABLE IF NOT EXISTS outbox (
  id BIGINT AUTO_INCREMENT NOT NULL PRIMARY KEY,
  event_id VARCHAR(64) NOT NULL UNIQUE,
  idem_key VARCHAR(191) NOT NULL UNIQUE,              -- (name, generation, event_type)
  worker_name VARCHAR(253) NOT NULL,
  generation BIGINT NOT NULL,
  event_type VARCHAR(32) NOT NULL,                    -- provision|deprovision|update
  destination VARCHAR(64) NOT NULL DEFAULT 'controller',
  destination_ref VARCHAR(255) NULL,
  payload_json JSON NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'pending',      -- pending|published|failed|dead
  attempts INT NOT NULL DEFAULT 0,
  next_attempt_at DATETIME(6) NULL,
  last_error VARCHAR(1024) NULL,
  published_at DATETIME(6) NULL,
  created_at DATETIME(6) NOT NULL,
  updated_at DATETIME(6) NOT NULL,
  KEY idx_outbox_pending (status, next_attempt_at)
);
