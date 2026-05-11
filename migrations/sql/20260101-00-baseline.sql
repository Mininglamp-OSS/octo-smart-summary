-- +migrate Up
CREATE TABLE IF NOT EXISTS `summary_chunk` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `task_id` bigint NOT NULL,
  `chunk_index` int NOT NULL,
  `participant_id` bigint DEFAULT NULL,
  `summary_source_id` bigint DEFAULT NULL,
  `msg_count` int NOT NULL DEFAULT '0',
  `msg_start_time` datetime DEFAULT NULL,
  `msg_end_time` datetime DEFAULT NULL,
  `chunk_summary` mediumtext COLLATE utf8mb4_unicode_ci NOT NULL,
  `token_used` int NOT NULL DEFAULT '0',
  `status` tinyint NOT NULL DEFAULT '0',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_task_id` (`task_id`),
  KEY `idx_summary_source_id` (`summary_source_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `summary_event` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `task_id` bigint NOT NULL,
  `status` tinyint NOT NULL,
  `progress` tinyint NOT NULL DEFAULT '0',
  `message` varchar(200) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_task_id` (`task_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `summary_participant` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `task_id` bigint NOT NULL,
  `user_id` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
  `user_name` varchar(100) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  `status` tinyint NOT NULL DEFAULT '0' COMMENT '0=pending 1=accepted 2=declined 3=processing 4=completed 5=submitted',
  `confirmed_at` datetime DEFAULT NULL,
  `personal_result_id` bigint DEFAULT NULL,
  `worker_started_at` datetime(6) DEFAULT NULL,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_task_id` (`task_id`),
  KEY `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `summary_personal_result` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `task_id` bigint NOT NULL,
  `participant_ref_id` bigint NOT NULL COMMENT 'FK summary_participant.id',
  `user_id` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL COMMENT 'redundant IM platform uid for per-user queries',
  `content` mediumtext COLLATE utf8mb4_unicode_ci NOT NULL,
  `citations_json` mediumtext COLLATE utf8mb4_unicode_ci,
  `msg_count` int NOT NULL DEFAULT '0',
  `total_token_used` int NOT NULL DEFAULT '0',
  `model_version` varchar(50) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  `worker_status` tinyint NOT NULL DEFAULT '0' COMMENT '0=PENDING 1=PROCESSING 2=COMPLETED 3=FAILED',
  `error_message` varchar(500) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `submitted_at` datetime(6) DEFAULT NULL COMMENT 'NULL=draft/unfinished NOT_NULL=submitted and visible',
  `generated_at` datetime(6) DEFAULT NULL,
  `created_at` datetime(6) NOT NULL,
  `updated_at` datetime(6) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_task_participant` (`task_id`,`participant_ref_id`),
  KEY `idx_task_id` (`task_id`),
  KEY `idx_user_id` (`task_id`,`user_id`),
  KEY `idx_submitted` (`task_id`,`submitted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `summary_result` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `task_id` bigint NOT NULL,
  `content` mediumtext COLLATE utf8mb4_unicode_ci NOT NULL,
  `citations_json` mediumtext COLLATE utf8mb4_unicode_ci,
  `team_citations_json` mediumtext COLLATE utf8mb4_unicode_ci,
  `total_msg_count` int NOT NULL DEFAULT '0',
  `total_token_used` int NOT NULL DEFAULT '0',
  `model_version` varchar(50) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  `version` int NOT NULL DEFAULT '1',
  `generated_at` datetime NOT NULL,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_task_id` (`task_id`),
  KEY `idx_task_version` (`task_id`,`version`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `summary_schedule` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `space_id` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  `creator_id` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
  `title` varchar(200) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  `summary_mode` tinyint NOT NULL,
  `cron_expr` varchar(50) COLLATE utf8mb4_unicode_ci NOT NULL,
  `time_range_type` tinyint NOT NULL COMMENT '1=24h,2=7d,3=30d,4=since_last_run',
  `source_config` json DEFAULT NULL,
  `participant_config` json DEFAULT NULL,
  `is_active` tinyint NOT NULL DEFAULT '1',
  `last_run_at` datetime DEFAULT NULL,
  `next_run_at` datetime DEFAULT NULL,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_space_active` (`space_id`,`is_active`),
  KEY `idx_next_run` (`is_active`,`next_run_at`),
  KEY `idx_deleted_at` (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `summary_source` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `task_id` bigint NOT NULL,
  `source_type` tinyint NOT NULL COMMENT '1=group,2=thread,3=direct',
  `source_id` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
  `source_name` varchar(200) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  `participant_id` bigint DEFAULT NULL,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_task_id` (`task_id`),
  KEY `idx_participant_id` (`participant_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `summary_task` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `task_no` varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL,
  `space_id` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  `creator_id` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
  `title` varchar(200) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  `summary_mode` tinyint NOT NULL COMMENT '1=by_group, 2=by_person',
  `time_range_start` datetime NOT NULL,
  `time_range_end` datetime NOT NULL,
  `status` tinyint NOT NULL DEFAULT '0' COMMENT '0=pending,1=waiting_confirm,2=processing,3=completed,4=failed,5=cancelled',
  `trigger_type` tinyint NOT NULL DEFAULT '1' COMMENT '1=manual,2=scheduled',
  `retry_count` tinyint NOT NULL DEFAULT '0',
  `error_message` varchar(500) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `schedule_id` bigint DEFAULT NULL,
  `processing_deadline` datetime DEFAULT NULL,
  `confirm_deadline` datetime DEFAULT NULL,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_task_no` (`task_no`),
  KEY `idx_space_status` (`space_id`,`status`),
  KEY `idx_status_retry` (`status`,`retry_count`),
  KEY `idx_schedule_id` (`schedule_id`),
  KEY `idx_deleted_at` (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +migrate Down
DROP TABLE IF EXISTS `summary_task`;
DROP TABLE IF EXISTS `summary_source`;
DROP TABLE IF EXISTS `summary_schedule`;
DROP TABLE IF EXISTS `summary_result`;
DROP TABLE IF EXISTS `summary_personal_result`;
DROP TABLE IF EXISTS `summary_participant`;
DROP TABLE IF EXISTS `summary_event`;
DROP TABLE IF EXISTS `summary_chunk`;
