-- Add summary_task.origin_from_derived — provenance flag for R11 Q2 (PR #190).
--
-- tier-3/tier-4 origin inheritance (agent_summary.go) can copy a channel id
-- that the refiner never specified: tier-4 reads the referenced task's
-- summary_source rows, and DERIVED rows (worker backfill of auto-selected
-- channels) may win. The PR body's owner-approved invariant "Derived
-- (auto-backfilled) sources are never exposed to API consumers" extends to
-- values derived FROM derived rows: list/detail projections mask
-- origin_channel_id/origin_channel_type to ("", 0) when this flag is set,
-- and the ?origin_channel_id= list filter excludes flagged rows (otherwise
-- the filter is a membership oracle).
--
-- DEFAULT 0: every origin written before this column existed was either
-- caller-provided or session-resolved — never derived-inherited. The
-- tier-4 code path itself ships in the same unmerged PR as this column, so
-- no production row can carry a derived-inherited origin yet.
--
-- Same caveat as 20260813-01 (R11 Q6): environments that deployed an
-- earlier head of this branch (79396e9/306588d shipped tier-4 BEFORE this
-- column) may hold rows whose origin came from a derived row with the flag
-- unset; such rows need a one-time manual cleanup since the schema cannot
-- distinguish them.
ALTER TABLE summary_task
    ADD COLUMN origin_from_derived TINYINT NOT NULL DEFAULT 0
    COMMENT '1=origin_channel_id was inherited from a derived (worker-backfilled) source row; masked on wire';
