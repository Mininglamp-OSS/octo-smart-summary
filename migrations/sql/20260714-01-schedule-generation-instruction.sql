-- +migrate Up
ALTER TABLE `summary_schedule`
  ADD COLUMN `generation_instruction` text COLLATE utf8mb4_unicode_ci NULL AFTER `title`;

-- +migrate Down
ALTER TABLE `summary_schedule`
  DROP COLUMN `generation_instruction`;
