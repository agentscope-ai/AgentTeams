-- R1 minimal resource schema: the workers table (58 §4.1, frozen DDL).
-- Forward-only. MySQL 8 / seekdb 5.7 compatible. No cross-schema FK (D52).
CREATE TABLE IF NOT EXISTS workers (
  name VARCHAR(253) NOT NULL PRIMARY KEY,           -- CRD name，公网主键（R1 无 worker_uid，R2 随 D35 引入）
  worker_name VARCHAR(255) NULL,
  team_slug VARCHAR(64) NULL,                        -- 首个命中 team 的缓存（C3）；R2 换 team_uid
  model VARCHAR(255) NULL,
  model_provider VARCHAR(255) NULL,
  subagent_model VARCHAR(255) NULL,
  llm_stream_first_content_timeout VARCHAR(64) NULL,
  llm_stream_idle_timeout VARCHAR(64) NULL,
  llm_max_retries VARCHAR(64) NULL,
  llm_backoff_base VARCHAR(64) NULL,
  llm_backoff_cap VARCHAR(64) NULL,
  llm_max_concurrent VARCHAR(64) NULL,
  llm_max_qpm VARCHAR(64) NULL,
  llm_rate_limit_pause VARCHAR(64) NULL,
  llm_rate_limit_jitter VARCHAR(64) NULL,
  llm_acquire_timeout VARCHAR(64) NULL,
  runtime VARCHAR(64) NULL,
  image VARCHAR(512) NULL,
  identity MEDIUMTEXT NULL,
  soul MEDIUMTEXT NULL,                              -- 配置文本，非文件 blob（D88 不冲突）
  agents MEDIUMTEXT NULL,
  skills_json JSON NULL,
  remote_skills_json JSON NULL,
  mcp_servers_json JSON NULL,
  package_ref VARCHAR(512) NULL,
  expose_json JSON NULL,
  channel_policy_json JSON NULL,
  resources_json JSON NULL,
  env_json JSON NULL,                                 -- 仅 admin/manager 写；非 admin 读 = NULL
  container_managed TINYINT(1) NOT NULL DEFAULT 1,
  desired_state VARCHAR(32) NULL,                     -- Running|Sleeping|Stopped
  observed_phase VARCHAR(32) NOT NULL DEFAULT 'Pending',
  observed_container_state VARCHAR(32) NULL,
  matrix_user_id VARCHAR(255) NULL,
  room_id VARCHAR(128) NULL,
  last_active_at DATETIME(6) NULL,
  agent_status VARCHAR(32) NULL,
  running_task_count INT NULL,
  last_run_at DATETIME(6) NULL,
  last_finish_at DATETIME(6) NULL,
  message VARCHAR(1024) NULL,
  generation BIGINT NOT NULL DEFAULT 1,               -- spec 变更计数（adapter 幂等键分量）
  spec_hash CHAR(64) NOT NULL,                        -- sha256(spec 序列化)，ETag/变更检测
  version BIGINT NOT NULL DEFAULT 1,                  -- 乐观锁
  created_at DATETIME(6) NOT NULL,
  updated_at DATETIME(6) NOT NULL,
  deleted_at DATETIME(6) NULL,
  KEY idx_workers_team (team_slug),
  KEY idx_workers_phase (observed_phase)
);
