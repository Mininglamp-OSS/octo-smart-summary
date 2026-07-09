-- +migrate Up
CREATE TABLE `summary_user_template` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `space_id` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  `user_id` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
  `template_id` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
  `label` varchar(100) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  `description` varchar(200) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  `is_custom` tinyint NOT NULL DEFAULT 0,
  `pattern` text COLLATE utf8mb4_unicode_ci NOT NULL,
  `sort_order` int NOT NULL DEFAULT 0,
  `deleted_at` datetime DEFAULT NULL,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_summary_user_template` (`space_id`,`user_id`,`template_id`),
  KEY `idx_summary_user_template_user` (`space_id`,`user_id`,`is_custom`,`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +migrate Down
DROP TABLE IF EXISTS `summary_user_template`;
