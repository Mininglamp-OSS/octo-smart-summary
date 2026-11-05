-- +migrate Up
-- MySQL DDL commits independently of the migration record. Guard each step
-- so a process/connection failure can resume the same migration ID safely.
-- Skip only the expected definition. An incompatible existing column must
-- fail with Duplicate column rather than be recorded as successfully migrated.
SET @summary_retry_ddl = IF(
    EXISTS (SELECT 1 FROM information_schema.COLUMNS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'summary_result'
              AND COLUMN_NAME = 'citation_owner_id' AND DATA_TYPE = 'varchar' AND CHARACTER_MAXIMUM_LENGTH = 64 AND IS_NULLABLE = 'NO' AND COLUMN_DEFAULT = ''),
    'SELECT 1',
    'ALTER TABLE summary_result ADD COLUMN citation_owner_id VARCHAR(64) NOT NULL DEFAULT '''''
);
PREPARE summary_retry_stmt FROM @summary_retry_ddl;
EXECUTE summary_retry_stmt;
DEALLOCATE PREPARE summary_retry_stmt;

-- A same-name index with a different key must fail, not silently pass.
SET @summary_retry_ddl = IF(
    (SELECT COUNT(*) = 3 AND
            SUM(NON_UNIQUE = 1 AND INDEX_TYPE = 'BTREE' AND SUB_PART IS NULL AND
                ((SEQ_IN_INDEX = 1 AND COLUMN_NAME = 'result_id') OR
                 (SEQ_IN_INDEX = 2 AND COLUMN_NAME = 'task_id') OR
                 (SEQ_IN_INDEX = 3 AND COLUMN_NAME = 'result_kind'))) = 3
     FROM information_schema.STATISTICS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'summary_agent_save_idempotency'
       AND INDEX_NAME = 'idx_save_result_task_kind'),
    'SELECT 1',
    'CREATE INDEX idx_save_result_task_kind ON summary_agent_save_idempotency (result_id, task_id, result_kind)'
);
PREPARE summary_retry_stmt FROM @summary_retry_ddl;
EXECUTE summary_retry_stmt;
DEALLOCATE PREPARE summary_retry_stmt;

-- Compatibility backfill for incremental deployments that already wrote trusted
-- optimization bindings before citation_owner_id was introduced. On a first
-- upgrade applying all three migrations before serving new code, the binding
-- columns have default values and this UPDATE normally matches no rows.
-- It does NOT reconstruct ownership for arbitrary pre-feature legacy results.
-- Backfill only versions with a server-authenticated optimization save binding.
-- CreatedBy alone is not proof of authorization to the underlying raw messages.
UPDATE summary_result AS r
JOIN summary_agent_save_idempotency AS s
  ON s.result_id = r.id AND s.task_id = r.task_id AND s.result_kind = 'team'
SET r.citation_owner_id = s.user_id
WHERE r.citation_owner_id = '';

-- +migrate Down
DROP INDEX idx_save_result_task_kind ON summary_agent_save_idempotency;
ALTER TABLE summary_result DROP COLUMN citation_owner_id;
