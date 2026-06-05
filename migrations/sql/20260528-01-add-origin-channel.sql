-- +migrate Up
ALTER TABLE `summary_task`
    ADD COLUMN `origin_channel_id` VARCHAR(64) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '' AFTER `schedule_id`,
    ADD COLUMN `origin_channel_type` TINYINT NOT NULL DEFAULT 0 AFTER `origin_channel_id`,
    ADD INDEX `idx_origin_channel` (`origin_channel_id`);

-- +migrate Down
ALTER TABLE `summary_task` DROP INDEX `idx_origin_channel`;
ALTER TABLE `summary_task` DROP COLUMN `origin_channel_type`;
ALTER TABLE `summary_task` DROP COLUMN `origin_channel_id`;
