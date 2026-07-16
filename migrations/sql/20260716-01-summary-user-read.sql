-- +migrate Up
CREATE TABLE `summary_user_read` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `task_id` bigint NOT NULL,
  `user_id` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
  `last_read_team_result_id` bigint DEFAULT NULL,
  `last_read_personal_version_id` bigint DEFAULT NULL,
  `read_at` datetime(3) DEFAULT NULL,
  `created_at` datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at` datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_summary_user_read_task_user` (`task_id`,`user_id`),
  KEY `idx_summary_user_read_user` (`user_id`,`task_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Existing visible summaries start as read so rollout does not create a wall
-- of historical red dots. Creator rows are inserted first; participant rows
-- then fill the remaining users and carry their own personal version cursor.
INSERT INTO `summary_user_read`
  (`task_id`,`user_id`,`last_read_team_result_id`,`last_read_personal_version_id`,`read_at`,`created_at`,`updated_at`)
SELECT t.`id`, t.`creator_id`, t.`current_result_id`, pr.`current_version_id`, NOW(3), NOW(3), NOW(3)
FROM `summary_task` t
LEFT JOIN `summary_personal_result` pr ON pr.`task_id`=t.`id` AND pr.`user_id`=t.`creator_id`
WHERE t.`deleted_at` IS NULL;

INSERT IGNORE INTO `summary_user_read`
  (`task_id`,`user_id`,`last_read_team_result_id`,`last_read_personal_version_id`,`read_at`,`created_at`,`updated_at`)
SELECT t.`id`, p.`user_id`, t.`current_result_id`, pr.`current_version_id`, NOW(3), NOW(3), NOW(3)
FROM `summary_task` t
JOIN `summary_participant` p ON p.`task_id`=t.`id`
LEFT JOIN `summary_personal_result` pr ON pr.`task_id`=t.`id` AND pr.`user_id`=p.`user_id`
WHERE t.`deleted_at` IS NULL;

-- +migrate Down
DROP TABLE IF EXISTS `summary_user_read`;
