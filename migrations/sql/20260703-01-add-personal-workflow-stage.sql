-- +migrate Up
ALTER TABLE `summary_personal_result`
  ADD COLUMN `workflow_stage` varchar(32) NOT NULL DEFAULT '';

-- +migrate Down
ALTER TABLE `summary_personal_result`
  DROP COLUMN `workflow_stage`;
