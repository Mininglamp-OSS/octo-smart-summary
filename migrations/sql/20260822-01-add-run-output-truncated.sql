-- +migrate Up
-- Truncation disclosure (stacked on PR #208): record that the MODEL'S OWN
-- OUTPUT hit its token ceiling, on the run row.
--
-- Why a NEW file rather than an edit to 20260820-01: sql-migrate keys applied
-- migrations by FILE NAME in `gorp_migrations` and does no checksum comparison
-- (internal/db/migrate.go uses EmbedFileSystemMigrationSource). 20260820-01 has
-- shipped, so any environment that has started summary-api since then already
-- recorded that id and will skip the file forever -- a column added inside it
-- would never exist in a deployed DB while the model struct lists it, giving
-- Error 1054 on every run INSERT. Migrations are immutable once merged.
--
-- Why NOT reuse `coverage_truncated`: that column means "the fetched message
-- pool was clipped" (we did not READ everything). This one means "the generated
-- text stopped mid-sentence" (we read everything but could not WRITE it all).
-- The remediation is opposite -- narrow the time range vs lower the detail
-- level -- and raising the fetch cap on an output-truncated run makes it worse.
-- They can co-occur, so one boolean would necessarily misreport a case.
--
-- TINYINT NOT NULL DEFAULT 0 (an INT default is legal on a non-empty table, so
-- there is no reason to accept the nullable form the JSON columns needed).
-- Defaulting to 0 is the safe direction here, unlike fetch_expected: it means
-- legacy rows disclose nothing extra rather than every legacy row disclosing a
-- truncation that never happened.
--
-- Serialized by the existing GET_LOCK('smart_summary_migration') in
-- internal/db/migrate.go.

ALTER TABLE `agent_summary_run`
    ADD COLUMN `output_truncated` TINYINT(1) NOT NULL DEFAULT 0 COMMENT 'a model completion on the answer path hit finish_reason=length; the deliverable is unfinished';

-- +migrate Down
ALTER TABLE `agent_summary_run`
    DROP COLUMN `output_truncated`;
