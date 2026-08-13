-- +migrate Up
-- R9 P1 (PR #190): flag worker-backfilled summary_source rows so every
-- user-visible projection (list/detail `sources`, share snapshot names,
-- agent reference-context bullets) can exclude them while tier-4 origin
-- derivation keeps reading them.
--
-- Backfill rows record the channels the pipeline auto-selected under the
-- creator's channel membership; the read authorization on this table is any
-- task participant, and the roster is mutable after generation (AddMembers
-- on Completed tasks). Surfacing auto-selected channels to later joiners
-- bridges two authorization domains. Flagging + projection filtering keeps
-- the footprint for origin inheritance without exposing it.
--
-- DEFAULT 0: all rows written before this column existed are
-- user-specified (create endpoint / bot create / schedule materialization)
-- and must remain visible.
ALTER TABLE summary_source
    ADD COLUMN derived TINYINT NOT NULL DEFAULT 0
    COMMENT '1=row written by worker source backfill (auto-selected channels); excluded from user-visible projections';

-- +migrate Down
ALTER TABLE summary_source DROP COLUMN derived;
