-- ops.idempotency_keys（审计轮补 —— Idempotency-Key 的存储落点）冻结 DDL。
CREATE TABLE IF NOT EXISTS idempotency_keys (
  idem_key VARCHAR(128) NOT NULL PRIMARY KEY,        -- 请求头 Idempotency-Key（客户端 UUID）
  worker_name VARCHAR(253) NOT NULL,
  response_status INT NOT NULL,                      -- 首次结果状态码（201/409…）
  response_body JSON NULL,                           -- 首次响应体（重放原样返回）
  created_at DATETIME(6) NOT NULL,
  expires_at DATETIME(6) NOT NULL,                   -- 保留 24h（冻结值）
  KEY idx_idem_expires (expires_at)
);
