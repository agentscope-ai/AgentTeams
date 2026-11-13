-- 与 T1 工作库 ops.audit_events（§6.2）逐字同构，唯一差异：source 默认值 'service'。
-- 15 号审计模型：service 只审计自己产生的判定（4xx/5xx → deny），不代记 controller/agent 侧。
CREATE TABLE IF NOT EXISTS audit_events (
  id BIGINT AUTO_INCREMENT NOT NULL PRIMARY KEY,
  request_id VARCHAR(128) NOT NULL,
  trace_id VARCHAR(128) NULL,
  actor_type VARCHAR(16) NOT NULL,
  actor_id VARCHAR(128) NOT NULL,
  on_behalf_of_type VARCHAR(16) NULL,
  on_behalf_of_id VARCHAR(128) NULL,
  team_uid CHAR(26) NULL,
  project_uid CHAR(26) NULL,
  task_id VARCHAR(64) NULL,
  action VARCHAR(128) NOT NULL,
  resource_version BIGINT NULL,
  decision VARCHAR(32) NOT NULL,                   -- allow|deny|pending_approval
  deny_reason VARCHAR(255) NULL,
  before_digest CHAR(64) NULL,
  after_digest CHAR(64) NULL,
  source VARCHAR(32) NOT NULL DEFAULT 'service',
  token_jti VARCHAR(128) NULL,
  client VARCHAR(191) NULL,
  created_at DATETIME(6) NOT NULL,
  KEY idx_audit_created (created_at),
  KEY idx_audit_team_created (team_uid, created_at),
  KEY idx_audit_project_created (project_uid, created_at),
  KEY idx_audit_task_created (task_id, created_at),
  KEY idx_audit_request (request_id)
);
