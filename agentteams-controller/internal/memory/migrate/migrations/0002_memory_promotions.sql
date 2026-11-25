-- 0002: memory_promotions - the promotion audit trail (W1 writes).
CREATE TABLE memory_promotions (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  anchor_id BIGINT NOT NULL,
  source_ref VARCHAR(512) NOT NULL,         -- PC Source id / contribution id
  entry_ref VARCHAR(512) NOT NULL,          -- PC artifact id/revision
  actor VARCHAR(256) NOT NULL,
  rationale TEXT,
  created_at DATETIME(6) NOT NULL
);
