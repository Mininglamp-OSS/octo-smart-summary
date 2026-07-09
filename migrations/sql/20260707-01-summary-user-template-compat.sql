-- +migrate Up
-- Compatibility migration for environments that already ran the early
-- 20260706-01-summary-user-template.sql before custom-template fields existed.
SET @stmt = (
  SELECT IF(COUNT(*) = 0,
    'ALTER TABLE `summary_user_template` ADD COLUMN `label` varchar(100) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT ''''',
    'SELECT 1'
  )
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'summary_user_template' AND COLUMN_NAME = 'label'
);
PREPARE stmt FROM @stmt;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @stmt = (
  SELECT IF(COUNT(*) = 0,
    'ALTER TABLE `summary_user_template` ADD COLUMN `description` varchar(200) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT ''''',
    'SELECT 1'
  )
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'summary_user_template' AND COLUMN_NAME = 'description'
);
PREPARE stmt FROM @stmt;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @stmt = (
  SELECT IF(COUNT(*) = 0,
    'ALTER TABLE `summary_user_template` ADD COLUMN `is_custom` tinyint NOT NULL DEFAULT 0',
    'SELECT 1'
  )
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'summary_user_template' AND COLUMN_NAME = 'is_custom'
);
PREPARE stmt FROM @stmt;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @stmt = (
  SELECT IF(COUNT(*) = 0,
    'ALTER TABLE `summary_user_template` ADD COLUMN `sort_order` int NOT NULL DEFAULT 0',
    'SELECT 1'
  )
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'summary_user_template' AND COLUMN_NAME = 'sort_order'
);
PREPARE stmt FROM @stmt;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @stmt = (
  SELECT IF(COUNT(*) = 0,
    'ALTER TABLE `summary_user_template` ADD COLUMN `deleted_at` datetime DEFAULT NULL',
    'SELECT 1'
  )
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'summary_user_template' AND COLUMN_NAME = 'deleted_at'
);
PREPARE stmt FROM @stmt;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @stmt = (
  SELECT IF(COUNT(*) = 0,
    'CREATE INDEX `idx_summary_user_template_user` ON `summary_user_template` (`space_id`, `user_id`, `is_custom`, `deleted_at`)',
    'SELECT 1'
  )
  FROM INFORMATION_SCHEMA.STATISTICS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'summary_user_template' AND INDEX_NAME = 'idx_summary_user_template_user'
);
PREPARE stmt FROM @stmt;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

-- +migrate Down
-- No-op: these columns are part of the canonical 20260706 schema for fresh databases.
SELECT 1;
