-- Add citations_json column to summary_result for citation tracking
ALTER TABLE `summary_result` ADD COLUMN `citations_json` MEDIUMTEXT DEFAULT NULL AFTER `content`;
