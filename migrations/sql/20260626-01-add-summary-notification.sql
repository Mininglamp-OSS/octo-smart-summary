-- +migrate Up
CREATE TABLE IF NOT EXISTS `summary_notification` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `task_id`       BIGINT       NOT NULL,
    `notify_kind`   VARCHAR(16)  NOT NULL COMMENT 'completed | failed',
    `recipient_uid` VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'per-recipient dedup: one row per (task, kind, uid)',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending | sent | failed',
    `attempt_count` INT          NOT NULL DEFAULT 0,
    `last_error`    VARCHAR(500)     NULL,
    `created_at`    DATETIME     NOT NULL,
    `updated_at`    DATETIME     NOT NULL,
    `sent_at`       DATETIME         NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_task_kind_uid` (`task_id`, `notify_kind`, `recipient_uid`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +migrate Down
DROP TABLE IF EXISTS `summary_notification`;
