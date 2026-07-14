-- +migrate Up
ALTER TABLE `summary_schedule`
  ADD COLUMN `generation_instruction` text NULL AFTER `title`;

-- +migrate Down
ALTER TABLE `summary_schedule`
  DROP COLUMN `generation_instruction`;
