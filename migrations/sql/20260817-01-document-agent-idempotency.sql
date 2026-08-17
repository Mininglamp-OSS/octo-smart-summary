-- +migrate Up
CREATE TABLE IF NOT EXISTS summary_document_agent_idempotency (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  space_id VARCHAR(64) NOT NULL,
  user_id VARCHAR(64) NOT NULL,
  idempotency_key VARCHAR(128) NOT NULL,
  request_hash CHAR(64) NOT NULL DEFAULT '',
  task_id BIGINT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_doc_agent_idempotency (space_id, user_id, idempotency_key),
  KEY idx_doc_agent_task_id (task_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +migrate Down
DROP TABLE IF EXISTS summary_document_agent_idempotency;
