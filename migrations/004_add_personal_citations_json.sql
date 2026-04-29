-- Add citations_json column to summary_personal_result for per-participant citation tracking (Phase 2)
ALTER TABLE `summary_personal_result` ADD COLUMN `citations_json` MEDIUMTEXT DEFAULT NULL AFTER `content`;
