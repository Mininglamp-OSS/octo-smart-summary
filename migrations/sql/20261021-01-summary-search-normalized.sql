-- +migrate Up
-- MySQL DDL commits independently of the migration record. Guard each step
-- so a process/connection failure can resume the same migration ID safely.
-- Skip only the expected definition. An incompatible existing column must
-- fail with Duplicate column rather than be recorded as successfully migrated.
SET @summary_retry_ddl = IF(
    EXISTS (SELECT 1 FROM information_schema.COLUMNS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'summary_task'
              AND COLUMN_NAME = 'search_title' AND DATA_TYPE = 'text' AND IS_NULLABLE = 'YES' AND COLLATION_NAME = 'utf8mb4_bin'),
    'SELECT 1',
    'ALTER TABLE summary_task ADD COLUMN search_title TEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL'
);
PREPARE summary_retry_stmt FROM @summary_retry_ddl;
EXECUTE summary_retry_stmt;
DEALLOCATE PREPARE summary_retry_stmt;

-- Skip only the expected definition. An incompatible existing column must
-- fail with Duplicate column rather than be recorded as successfully migrated.
SET @summary_retry_ddl = IF(
    EXISTS (SELECT 1 FROM information_schema.COLUMNS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'summary_task'
              AND COLUMN_NAME = 'search_topic' AND DATA_TYPE = 'text' AND IS_NULLABLE = 'YES' AND COLLATION_NAME = 'utf8mb4_bin'),
    'SELECT 1',
    'ALTER TABLE summary_task ADD COLUMN search_topic TEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL'
);
PREPARE summary_retry_stmt FROM @summary_retry_ddl;
EXECUTE summary_retry_stmt;
DEALLOCATE PREPARE summary_retry_stmt;

-- Go backfill runs under the migration lock before API/worker startup.
-- SQL LOWER/collations cannot reproduce Go Unicode width/full case folding.

-- +migrate Down
ALTER TABLE summary_task DROP COLUMN search_title, DROP COLUMN search_topic;
