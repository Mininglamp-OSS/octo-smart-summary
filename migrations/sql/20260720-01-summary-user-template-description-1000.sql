-- +migrate Up
ALTER TABLE `summary_user_template` MODIFY COLUMN `description` VARCHAR(1000) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '';

-- +migrate Down
ALTER TABLE `summary_user_template` MODIFY COLUMN `description` VARCHAR(200) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '';
