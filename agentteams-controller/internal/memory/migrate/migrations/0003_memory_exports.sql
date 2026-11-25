-- 0003: memory_exports - completed exports (also written to storage_refs).
CREATE TABLE memory_exports (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  anchor_id BIGINT NOT NULL,
  format VARCHAR(16) NOT NULL,
  object_key VARCHAR(1024) NOT NULL,
  sha256 CHAR(64) NOT NULL,
  created_at DATETIME(6) NOT NULL
);
