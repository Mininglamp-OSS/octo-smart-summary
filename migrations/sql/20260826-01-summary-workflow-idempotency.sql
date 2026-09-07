-- +migrate Up
--
-- A user-owned idempotency binding for summary workflow creation. The same
-- table is used by POST /summaries and the future Agent workflow tools.

CREATE TABLE summary_workflow_idempotency (
    id BIGINT NOT NULL AUTO_INCREMENT,
    space_id VARCHAR(64) NOT NULL,
    user_id VARCHAR(64) NOT NULL,
    -- Idempotency keys are opaque and byte-sensitive: Retry-A and retry-a
    -- must remain distinct even though the table default is case-insensitive.
    idempotency_key VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
    request_hash CHAR(64) NOT NULL,
    task_id BIGINT NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_summary_workflow_idempotency (space_id, user_id, idempotency_key),
    KEY idx_summary_workflow_idempotency_task (task_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +migrate Down
DROP TABLE IF EXISTS summary_workflow_idempotency;
