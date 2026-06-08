-- +migrate Up
-- PR#62 r12: heal half-applied MySQL DDL states left behind when sql-migrate
-- marked 20260608-01/03/06 unapplied after a later statement failed. MySQL DDL
-- auto-commits, so re-runs must converge idempotently via information_schema +
-- PREPARE/EXECUTE instead of unsupported IF [NOT] EXISTS column/index syntax.
-- Future rule: any migration that contains DDL must contain exactly one DDL
-- statement per file. This repair file is the exception because it converges
-- multiple historical half-applied states into the already-released target
-- schema without modifying published migrations.
SET @pr62_r12_has_live_schedule_id = (
    SELECT COUNT(*)
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'summary_task'
      AND COLUMN_NAME = 'live_schedule_id'
);

SET @pr62_r12_sql = IF(
    @pr62_r12_has_live_schedule_id = 0,
    'ALTER TABLE summary_task
    ADD COLUMN live_schedule_id BIGINT
        GENERATED ALWAYS AS (
            CASE WHEN deleted_at IS NULL AND schedule_id IS NOT NULL
                 THEN schedule_id
                 ELSE NULL
            END
        ) STORED',
    'SELECT 1'
);

PREPARE stmt FROM @pr62_r12_sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @pr62_r12_has_uk_live_schedule_binding = (
    SELECT COUNT(*)
    FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'summary_task'
      AND INDEX_NAME = 'uk_live_schedule_binding'
);

SET @pr62_r12_sql = IF(
    @pr62_r12_has_uk_live_schedule_binding = 0,
    'ALTER TABLE summary_task
    ADD UNIQUE KEY uk_live_schedule_binding (live_schedule_id)',
    'SELECT 1'
);

PREPARE stmt FROM @pr62_r12_sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @pr62_r12_has_anchor_dom = (
    SELECT COUNT(*)
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'summary_schedule'
      AND COLUMN_NAME = 'anchor_dom'
);

SET @pr62_r12_sql = IF(
    @pr62_r12_has_anchor_dom = 0,
    'ALTER TABLE summary_schedule
    ADD COLUMN anchor_dom TINYINT NOT NULL DEFAULT 0 AFTER day_of_month',
    'SELECT 1'
);

PREPARE stmt FROM @pr62_r12_sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @pr62_r12_has_uk_summary_participant_task_user = (
    SELECT COUNT(*)
    FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'summary_participant'
      AND INDEX_NAME = 'uk_summary_participant_task_user'
);

SET @pr62_r12_sql = IF(
    @pr62_r12_has_uk_summary_participant_task_user = 0,
    'CREATE UNIQUE INDEX `uk_summary_participant_task_user` ON `summary_participant` (`task_id`, `user_id`)',
    'SELECT 1'
);

PREPARE stmt FROM @pr62_r12_sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

-- +migrate Down
-- No-op: this migration only converges partially applied historical DDL to the
-- already-published target schema, so rolling it back would incorrectly remove
-- schema objects that earlier migrations own.
SELECT 1;
