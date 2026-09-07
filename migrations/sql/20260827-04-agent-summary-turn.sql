-- +migrate Up
-- Durable request idempotency and lease records for workspace turns.
-- IF NOT EXISTS supports databases that already ran the former all-in-one
-- 20260827-01 migration.

CREATE TABLE IF NOT EXISTS `agent_summary_turn` (
    `id` BIGINT NOT NULL AUTO_INCREMENT,
    `space_id` VARCHAR(64) NOT NULL,
    `user_id` VARCHAR(64) NOT NULL,
    `session_id` VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
    `request_id` VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
    `request_hash` CHAR(64) NOT NULL,
    `scope_version` INT NOT NULL,
    `status` VARCHAR(16) NOT NULL,
    `attempt` INT NOT NULL DEFAULT 1,
    `lease_expires_at` DATETIME(3) NULL,
    `run_id` VARCHAR(64) NOT NULL DEFAULT '',
    `response_message_id` BIGINT NOT NULL DEFAULT 0,
    `result_type` VARCHAR(32) NOT NULL DEFAULT '',
    `response_json` JSON NULL,
    `error_code` VARCHAR(64) NOT NULL DEFAULT '',
    `created_at` DATETIME(3) NOT NULL,
    `updated_at` DATETIME(3) NOT NULL,
    `completed_at` DATETIME(3) NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_agent_summary_turn_request` (`space_id`, `user_id`, `session_id`, `request_id`),
    KEY `idx_agent_summary_turn_owner_session` (`space_id`, `user_id`, `session_id`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

SET @agent_summary_turn_owner_index_exists = (
    SELECT COUNT(*)
    FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'agent_summary_turn'
      AND INDEX_NAME = 'idx_agent_summary_turn_owner_session'
);

SET @agent_summary_turn_owner_index_sql = IF(
    @agent_summary_turn_owner_index_exists = 0,
    'ALTER TABLE `agent_summary_turn` ADD KEY `idx_agent_summary_turn_owner_session` (`space_id`, `user_id`, `session_id`, `status`), ALGORITHM=INPLACE, LOCK=NONE',
    'SELECT 1'
);

PREPARE agent_summary_turn_owner_index_stmt FROM @agent_summary_turn_owner_index_sql;
EXECUTE agent_summary_turn_owner_index_stmt;
DEALLOCATE PREPARE agent_summary_turn_owner_index_stmt;

-- +migrate Down
-- Down is destructive and is not used by the production migration runner.
DROP TABLE IF EXISTS `agent_summary_turn`;
