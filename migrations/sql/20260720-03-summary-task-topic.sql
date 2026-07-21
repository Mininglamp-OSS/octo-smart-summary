-- +migrate Up
ALTER TABLE `summary_task` ADD COLUMN `topic` VARCHAR(1300) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '' AFTER `title`;
UPDATE `summary_task` SET `topic` = `title` WHERE `topic` = '';

-- +migrate Down
ALTER TABLE `summary_task` DROP COLUMN `topic`;
