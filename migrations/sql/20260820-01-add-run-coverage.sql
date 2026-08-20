-- +migrate Up
-- SS-07 (PR #203): record what a run actually fetched, on the run row.
--
-- Why a NEW file rather than an edit to 20260817-01: sql-migrate keys applied
-- migrations by FILE NAME in `gorp_migrations` and does no checksum comparison
-- (internal/db/migrate.go uses EmbedFileSystemMigrationSource). 20260817-01
-- shipped to main in PR #197, so any environment that has started summary-api
-- since then already recorded that id and will skip the file forever. Columns
-- added inside its CREATE TABLE would therefore never exist in a deployed DB,
-- while CreateOrGetRun's INSERT lists them -- Error 1054, swallowed by
-- maybePersistSummaryRun, which silently disables the entire V2 contract while
-- chat keeps answering 200. Migrations are immutable once merged.
--
-- The finish gate needs to distinguish three states that the frozen citation
-- artifact structurally cannot express, because artifact.ChannelCount only
-- counts channels that PRODUCED messages:
--   * never measured  (no fetch phase ran)   -> disclose "not measured"
--   * measured, empty (quiet channel fetched OK) -> not a gap
--   * measured, failed/absent                -> a real gap
-- Hence coverage_measured as an explicit flag plus per-channel id sets.
--
-- JSON columns cannot carry a DEFAULT literal in MySQL, so on a table that
-- already has rows `ADD COLUMN ... JSON NOT NULL` fails under strict mode.
-- They are added NULL-able; decodeChannels (internal/agent/summaryrun/store.go)
-- already treats NULL/"" as the empty set, and CreateOrGetRun writes "[]" for
-- every new row.
--
-- dropped_messages is deliberately NOT NULL DEFAULT 0, unlike the JSON columns:
-- AddDroppedMessages increments it with `dropped_messages + ?`, and on a
-- nullable column NULL + n is NULL, so the counter would silently stay unset
-- and a heavily-truncated run would come out COMPLETE -- exactly the failure
-- the gate exists to prevent. An INT default is legal on a non-empty table, so
-- there is no reason to accept the nullable form here.
--
-- fetch_expected defaults to 1 so pre-existing rows keep being coverage-judged;
-- only SS-08b confident rewrites (whose fetch tools are physically removed) set
-- it to 0. Defaulting to 0 would silence the gate on every legacy row.
--
-- Serialized by the existing GET_LOCK('smart_summary_migration') in
-- internal/db/migrate.go.

ALTER TABLE `agent_summary_run`
    ADD COLUMN `coverage_measured`  TINYINT(1) NOT NULL DEFAULT 0 COMMENT 'whether channel fetch coverage was actually observed',
    ADD COLUMN `attempted_channels` JSON       NULL              COMMENT 'unique channel ids passed to fetch_channel',
    ADD COLUMN `succeeded_channels` JSON       NULL              COMMENT 'attempted channels whose latest fetch succeeded, including quiet channels',
    ADD COLUMN `failed_channels`    JSON       NULL              COMMENT 'attempted channels whose latest fetch failed',
    ADD COLUMN `coverage_truncated` TINYINT(1) NOT NULL DEFAULT 0 COMMENT 'at least one successful fetch hit its message cap',
    ADD COLUMN `dropped_messages`   INT        NOT NULL DEFAULT 0 COMMENT 'messages discarded before summarization, including frozen-manifest misses',
    ADD COLUMN `fetch_expected`     TINYINT(1) NOT NULL DEFAULT 1 COMMENT 'whether this turn was supposed to fetch at all; 0 for SS-08b confident rewrites',
    ADD COLUMN `discovered_channels` JSON      NULL              COMMENT 'in-scope channels the run found but no UI selection pinned (open scope under-fetch detection)';

-- +migrate Down
ALTER TABLE `agent_summary_run`
    DROP COLUMN `coverage_measured`,
    DROP COLUMN `attempted_channels`,
    DROP COLUMN `succeeded_channels`,
    DROP COLUMN `failed_channels`,
    DROP COLUMN `coverage_truncated`,
    DROP COLUMN `dropped_messages`,
    DROP COLUMN `fetch_expected`,
    DROP COLUMN `discovered_channels`;
