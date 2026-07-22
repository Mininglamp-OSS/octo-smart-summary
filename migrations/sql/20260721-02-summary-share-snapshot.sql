-- +migrate Up
CREATE TABLE `summary_share_snapshot` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `task_id` BIGINT NOT NULL,
  `task_no` VARCHAR(32) NOT NULL,
  `space_id` VARCHAR(64) NOT NULL,
  `creator_id` VARCHAR(64) NOT NULL,
  `idempotency_key` VARCHAR(64) NOT NULL,
  `request_hash` CHAR(64) NOT NULL,
  `title` VARCHAR(2300) NOT NULL DEFAULT '',
  `source_name` VARCHAR(500) NOT NULL DEFAULT '',
  `source_count` INT NOT NULL DEFAULT 0,
  `participant_count` INT NOT NULL DEFAULT 0,
  `message_count` INT NOT NULL DEFAULT 0,
  `time_range_start` DATETIME NOT NULL,
  `time_range_end` DATETIME NOT NULL,
  `summary_mode` TINYINT NOT NULL,
  `result_version` INT NOT NULL DEFAULT 1,
  `preview` TEXT NOT NULL,
  `content` MEDIUMTEXT NOT NULL,
  `created_at` DATETIME NOT NULL,
  `updated_at` DATETIME NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_summary_share_idempotency` (`space_id`, `creator_id`, `idempotency_key`),
  KEY `idx_summary_share_task` (`task_id`)
);

CREATE TABLE `summary_share_grant` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `snapshot_id` BIGINT NOT NULL,
  `share_id` VARCHAR(64) NOT NULL,
  `channel_id` VARCHAR(128) NOT NULL,
  `channel_type` TINYINT NOT NULL,
  `status` TINYINT NOT NULL DEFAULT 1,
  `revoked_at` DATETIME NULL,
  `created_at` DATETIME NOT NULL,
  `updated_at` DATETIME NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_summary_share_id` (`share_id`),
  UNIQUE KEY `uk_summary_share_target` (`snapshot_id`, `channel_id`, `channel_type`),
  KEY `idx_summary_share_snapshot` (`snapshot_id`)
);

-- +migrate Down
DROP TABLE IF EXISTS `summary_share_grant`;
DROP TABLE IF EXISTS `summary_share_snapshot`;
