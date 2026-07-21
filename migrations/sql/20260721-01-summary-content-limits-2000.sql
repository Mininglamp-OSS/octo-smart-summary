-- +migrate Up
ALTER TABLE `summary_user_template` MODIFY COLUMN `description` VARCHAR(2000) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '';
ALTER TABLE `summary_task` MODIFY COLUMN `topic` VARCHAR(2300) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '';
ALTER TABLE `summary_task` MODIFY COLUMN `title` VARCHAR(2300) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '';
ALTER TABLE `summary_schedule` MODIFY COLUMN `title` VARCHAR(2300) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '';

-- +migrate Down
ALTER TABLE `summary_user_template` MODIFY COLUMN `description` VARCHAR(1000) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '';
ALTER TABLE `summary_task` MODIFY COLUMN `topic` VARCHAR(1300) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '';
ALTER TABLE `summary_task` MODIFY COLUMN `title` VARCHAR(1300) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '';
ALTER TABLE `summary_schedule` MODIFY COLUMN `title` VARCHAR(1300) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '';
