-- +migrate Up
CREATE TABLE summary_generation_run (
  id VARCHAR(36) NOT NULL PRIMARY KEY,
  space_id VARCHAR(64) NOT NULL,
  task_id BIGINT NOT NULL,
  content_id VARCHAR(512) NOT NULL,
  actor_id VARCHAR(64) NOT NULL,
  operation_type VARCHAR(32) NOT NULL,
  executor VARCHAR(16) NOT NULL,
  scope VARCHAR(16) NOT NULL,
  parent_generation_id VARCHAR(36) NULL,
  idempotency_hash CHAR(64) NOT NULL,
  request_hash CHAR(64) NOT NULL,
  active_slot CHAR(64) NULL,
  schedule_id BIGINT NULL,
  scheduled_for DATETIME(6) NULL,
  effective_at DATETIME(6) NOT NULL,
  base_version_id VARCHAR(768) NOT NULL,
  base_content_revision BIGINT NOT NULL,
  config_revision BIGINT NOT NULL,
  input_json JSON NOT NULL,
  status VARCHAR(24) NOT NULL,
  stage VARCHAR(64) NOT NULL DEFAULT '',
  lease_until DATETIME(6) NULL,
  execution_token BIGINT NOT NULL DEFAULT 0,
  cancel_requested BOOLEAN NOT NULL DEFAULT FALSE,
  output_version_id VARCHAR(768) NOT NULL DEFAULT '',
  applied BOOLEAN NOT NULL DEFAULT FALSE,
  conflict_reason VARCHAR(64) NOT NULL DEFAULT '',
  error_code VARCHAR(64) NOT NULL DEFAULT '',
  created_at DATETIME(6) NOT NULL,
  updated_at DATETIME(6) NOT NULL,
  UNIQUE KEY uk_generation_idempotency (idempotency_hash),
  UNIQUE KEY uk_generation_active_slot (active_slot),
  UNIQUE KEY uk_generation_schedule_slot (schedule_id, scheduled_for),
  KEY idx_generation_task (space_id, task_id),
  KEY idx_generation_recovery (status, lease_until),
  KEY idx_generation_parent (parent_generation_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE summary_content_audit (
  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  space_id VARCHAR(64) NOT NULL,
  task_id BIGINT NOT NULL,
  content_id VARCHAR(512) NOT NULL,
  version_id VARCHAR(768) NOT NULL,
  content_revision BIGINT NOT NULL,
  operation_type VARCHAR(32) NOT NULL,
  actor_id VARCHAR(64) NOT NULL,
  source_version_id VARCHAR(768) NOT NULL DEFAULT '',
  source_content_revision BIGINT NOT NULL DEFAULT 0,
  created_at DATETIME(6) NOT NULL,
  KEY idx_content_audit_task (task_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- +migrate Down
SELECT 1;
