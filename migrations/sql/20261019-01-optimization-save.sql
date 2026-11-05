-- +migrate Up
-- MySQL DDL commits independently of the migration record. Guard each step
-- so a process/connection failure can resume the same migration ID safely.
-- Skip only the expected definition. An incompatible existing column must
-- fail with Duplicate column rather than be recorded as successfully migrated.
SET @summary_retry_ddl = IF(
    EXISTS (SELECT 1 FROM information_schema.COLUMNS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'summary_agent_save_idempotency'
              AND COLUMN_NAME = 'result_id' AND DATA_TYPE = 'bigint' AND COLUMN_TYPE NOT LIKE '%unsigned%' AND IS_NULLABLE = 'NO' AND COLUMN_DEFAULT = '0'),
    'SELECT 1',
    'ALTER TABLE summary_agent_save_idempotency ADD COLUMN result_id BIGINT NOT NULL DEFAULT 0'
);
PREPARE summary_retry_stmt FROM @summary_retry_ddl;
EXECUTE summary_retry_stmt;
DEALLOCATE PREPARE summary_retry_stmt;

-- Skip only the expected definition. An incompatible existing column must
-- fail with Duplicate column rather than be recorded as successfully migrated.
SET @summary_retry_ddl = IF(
    EXISTS (SELECT 1 FROM information_schema.COLUMNS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'summary_agent_save_idempotency'
              AND COLUMN_NAME = 'result_version' AND DATA_TYPE = 'int' AND COLUMN_TYPE NOT LIKE '%unsigned%' AND IS_NULLABLE = 'NO' AND COLUMN_DEFAULT = '0'),
    'SELECT 1',
    'ALTER TABLE summary_agent_save_idempotency ADD COLUMN result_version INT NOT NULL DEFAULT 0'
);
PREPARE summary_retry_stmt FROM @summary_retry_ddl;
EXECUTE summary_retry_stmt;
DEALLOCATE PREPARE summary_retry_stmt;

-- Skip only the expected definition. An incompatible existing column must
-- fail with Duplicate column rather than be recorded as successfully migrated.
SET @summary_retry_ddl = IF(
    EXISTS (SELECT 1 FROM information_schema.COLUMNS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'summary_agent_save_idempotency'
              AND COLUMN_NAME = 'result_kind' AND DATA_TYPE = 'varchar' AND CHARACTER_MAXIMUM_LENGTH = 24 AND IS_NULLABLE = 'NO' AND COLUMN_DEFAULT = ''),
    'SELECT 1',
    'ALTER TABLE summary_agent_save_idempotency ADD COLUMN result_kind VARCHAR(24) NOT NULL DEFAULT '''''
);
PREPARE summary_retry_stmt FROM @summary_retry_ddl;
EXECUTE summary_retry_stmt;
DEALLOCATE PREPARE summary_retry_stmt;

-- Skip only the expected definition. An incompatible existing column must
-- fail with Duplicate column rather than be recorded as successfully migrated.
SET @summary_retry_ddl = IF(
    EXISTS (SELECT 1 FROM information_schema.COLUMNS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'summary_result'
              AND COLUMN_NAME = 'snapshot_json' AND DATA_TYPE = 'mediumtext' AND IS_NULLABLE = 'YES'),
    'SELECT 1',
    'ALTER TABLE summary_result ADD COLUMN snapshot_json MEDIUMTEXT NULL'
);
PREPARE summary_retry_stmt FROM @summary_retry_ddl;
EXECUTE summary_retry_stmt;
DEALLOCATE PREPARE summary_retry_stmt;

-- Skip only the expected definition. An incompatible existing column must
-- fail with Duplicate column rather than be recorded as successfully migrated.
SET @summary_retry_ddl = IF(
    EXISTS (SELECT 1 FROM information_schema.COLUMNS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'summary_personal_result_version'
              AND COLUMN_NAME = 'snapshot_json' AND DATA_TYPE = 'mediumtext' AND IS_NULLABLE = 'YES'),
    'SELECT 1',
    'ALTER TABLE summary_personal_result_version ADD COLUMN snapshot_json MEDIUMTEXT NULL'
);
PREPARE summary_retry_stmt FROM @summary_retry_ddl;
EXECUTE summary_retry_stmt;
DEALLOCATE PREPARE summary_retry_stmt;

-- +migrate Down
ALTER TABLE summary_result DROP COLUMN snapshot_json;
ALTER TABLE summary_personal_result_version DROP COLUMN snapshot_json;
ALTER TABLE summary_agent_save_idempotency DROP COLUMN result_id, DROP COLUMN result_version, DROP COLUMN result_kind;
