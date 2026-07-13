-- +migrate Up
CREATE TABLE IF NOT EXISTS `summary_personal_result_version` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `task_id` bigint NOT NULL,
  `participant_ref_id` bigint NOT NULL,
  `user_id` varchar(64) NOT NULL,
  `content` mediumtext NOT NULL,
  `citations_json` mediumtext NULL,
  `msg_count` int NOT NULL DEFAULT 0,
  `total_token_used` int NOT NULL DEFAULT 0,
  `model_version` varchar(50) NOT NULL DEFAULT '',
  `version` int NOT NULL DEFAULT 1,
  `operation_type` varchar(32) NOT NULL DEFAULT 'generate',
  `operation_note` text NULL,
  `parent_version_id` bigint NULL,
  `created_by` varchar(64) NOT NULL DEFAULT '',
  `generated_at` datetime(6) NOT NULL,
  `created_at` datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `updated_at` datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (`id`),
  KEY `idx_personal_result_version_task_user_version` (`task_id`, `user_id`, `version`),
  KEY `idx_personal_result_version_parent` (`parent_version_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- +migrate Down
DROP TABLE IF EXISTS `summary_personal_result_version`;
