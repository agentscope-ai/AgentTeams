-- 0008: memory_export_jobs - async export job state; the blob goes to
-- the object store, never into the database.
CREATE TABLE memory_export_jobs (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  anchor_id BIGINT NOT NULL,
  format VARCHAR(16) NOT NULL,
  status VARCHAR(16) NOT NULL,              -- pending|running|done|failed
  object_key VARCHAR(1024),
  sha256 CHAR(64),
  error TEXT,
  created_at DATETIME(6) NOT NULL,
  updated_at DATETIME(6) NOT NULL
);
