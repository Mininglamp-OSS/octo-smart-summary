-- +migrate Up
-- Durable folded state for one unified summary workspace.
-- IF NOT EXISTS supports databases that already ran the former all-in-one
-- 20260827-01 migration.

CREATE TABLE IF NOT EXISTS `agent_summary_session` (
    `id` BIGINT NOT NULL AUTO_INCREMENT,
    `space_id` VARCHAR(64) NOT NULL,
    `user_id` VARCHAR(64) NOT NULL,
    `session_id` VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
    `agent_session_id` VARCHAR(80) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL DEFAULT '',
    `contract_version` VARCHAR(16) NOT NULL DEFAULT '1',
    `state` VARCHAR(32) NOT NULL DEFAULT 'idle',
    `state_version` BIGINT NOT NULL DEFAULT 1,
    `scope_version` INT NOT NULL DEFAULT 1,
    `scope_json` JSON NULL,
    `scope_hash` CHAR(64) NOT NULL DEFAULT '',
    `active_turn_id` BIGINT NOT NULL DEFAULT 0,
    `artifact_version` INT NOT NULL DEFAULT 0,
    `latest_preview_message_id` BIGINT NOT NULL DEFAULT 0,
    `latest_preview_saved_task_id` BIGINT NOT NULL DEFAULT 0,
    `pending_proposal_version` INT NOT NULL DEFAULT 0,
    `pending_proposal_status` VARCHAR(16) NOT NULL DEFAULT '',
    `pending_proposal_token` VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL DEFAULT '',
    `pending_proposal_json` JSON NULL,
    `pending_proposal_message_id` BIGINT NOT NULL DEFAULT 0,
    `pending_proposal_scope_version` INT NOT NULL DEFAULT 0,
    `pending_proposal_task_id` BIGINT NOT NULL DEFAULT 0,
    `workflow_task_id` BIGINT NOT NULL DEFAULT 0,
    `workflow_scope` VARCHAR(16) NOT NULL DEFAULT '',
    `workflow_scope_version` INT NOT NULL DEFAULT 0,
    `workflow_started_message_id` BIGINT NOT NULL DEFAULT 0,
    `workflow_terminal_message_id` BIGINT NOT NULL DEFAULT 0,
    `expires_at` DATETIME(3) NULL,
    `created_at` DATETIME(3) NOT NULL,
    `updated_at` DATETIME(3) NOT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_agent_summary_session` (`space_id`, `user_id`, `session_id`),
    KEY `idx_agent_summary_session_agent_identity` (`user_id`, `agent_session_id`),
    KEY `idx_agent_summary_session_workflow` (`workflow_task_id`),
    KEY `idx_agent_summary_session_expires` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +migrate Down
-- Down is destructive and is not used by the production migration runner.
DROP TABLE IF EXISTS `agent_summary_session`;
