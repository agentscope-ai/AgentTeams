-- 58 §4.6，ops.upgrade_leases（D53）。kernel 是 ops.upgrade_leases 唯一 lease owner（55 号）。
CREATE TABLE IF NOT EXISTS upgrade_leases (
  scope VARCHAR(128) NOT NULL PRIMARY KEY,            -- kernel:upgrade
  holder_id VARCHAR(128) NOT NULL,
  lease_until DATETIME(6) NOT NULL,
  version BIGINT NOT NULL DEFAULT 1,
  updated_at DATETIME(6) NOT NULL
);
