-- +migrate Up
-- Workspace metadata on agent_message. Keep this hot-table change separate
-- from table creation so a failure cannot leave one migration half-applied.
-- Do not change session_id collation here: it is indexed on the live chat
-- table and would force a blocking COPY rebuild.
--
-- The conditional keeps this migration compatible with environments that
-- already ran the former all-in-one 20260827-01 migration.

SET @agent_message_workspace_exists = (
    SELECT COUNT(*)
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'agent_message'
      AND COLUMN_NAME = 'space_id'
);

SET @agent_message_workspace_sql = IF(
    @agent_message_workspace_exists = 0,
    'ALTER TABLE `agent_message`
        ADD COLUMN `space_id` VARCHAR(64) NOT NULL DEFAULT '''' AFTER `id`,
        ADD COLUMN `turn_id` BIGINT NOT NULL DEFAULT 0 AFTER `user_id`,
        ADD COLUMN `result_type` VARCHAR(32) NOT NULL DEFAULT '''' AFTER `output_truncated`,
        ADD COLUMN `response_payload_json` JSON NULL AFTER `result_type`,
        ADD COLUMN `scope_version` INT NOT NULL DEFAULT 0 AFTER `response_payload_json`,
        ADD COLUMN `artifact_version` INT NOT NULL DEFAULT 0 AFTER `scope_version`,
        ADD COLUMN `snapshot_version` INT NOT NULL DEFAULT 0 AFTER `artifact_version`,
        ADD COLUMN `parent_message_id` BIGINT NOT NULL DEFAULT 0 AFTER `snapshot_version`,
        ADD COLUMN `saved_task_id` BIGINT NOT NULL DEFAULT 0 AFTER `parent_message_id`,
        ADD KEY `idx_agent_message_workspace_created` (`space_id`, `user_id`, `session_id`, `id`),
        ADD KEY `idx_agent_message_turn` (`turn_id`),
        ALGORITHM=INPLACE,
        LOCK=NONE',
    'SELECT 1'
);

PREPARE agent_message_workspace_stmt FROM @agent_message_workspace_sql;
EXECUTE agent_message_workspace_stmt;
DEALLOCATE PREPARE agent_message_workspace_stmt;

-- +migrate Down
-- Down is destructive and is not used by the production migration runner.
ALTER TABLE `agent_message`
    DROP KEY `idx_agent_message_turn`,
    DROP KEY `idx_agent_message_workspace_created`,
    DROP COLUMN `saved_task_id`,
    DROP COLUMN `parent_message_id`,
    DROP COLUMN `snapshot_version`,
    DROP COLUMN `artifact_version`,
    DROP COLUMN `scope_version`,
    DROP COLUMN `response_payload_json`,
    DROP COLUMN `result_type`,
    DROP COLUMN `turn_id`,
    DROP COLUMN `space_id`,
    ALGORITHM=INPLACE,
    LOCK=NONE;
