-- Add team_citations_json column to summary_result for team-level participant citations
ALTER TABLE `summary_result` ADD COLUMN `team_citations_json` MEDIUMTEXT DEFAULT NULL AFTER `citations_json`;
