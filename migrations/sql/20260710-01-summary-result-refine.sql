-- +migrate Up
ALTER TABLE `summary_result`
  ADD COLUMN `operation_type` varchar(32) NOT NULL DEFAULT 'generate' AFTER `version`,
  ADD COLUMN `operation_note` text NULL AFTER `operation_type`,
  ADD COLUMN `parent_result_id` bigint NULL AFTER `operation_note`,
  ADD COLUMN `created_by` varchar(64) NOT NULL DEFAULT '' AFTER `parent_result_id`;

-- +migrate Down
ALTER TABLE `summary_result`
  DROP COLUMN `created_by`,
  DROP COLUMN `parent_result_id`,
  DROP COLUMN `operation_note`,
  DROP COLUMN `operation_type`;
