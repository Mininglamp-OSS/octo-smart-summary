-- +migrate Up
-- Data hygiene for the interval-only schedule contract.
--
-- Three things, all reversible/traceable (see Down + report for affected-row counts):
--
-- 1. Multi-source dirty rows: a schedule should have exactly one recurrence
--    source. Historically a row could have cron_expr AND interval_days AND/or
--    interval_months set at once, which the scheduler now treats as invalid and
--    would disable. Resolve by applying the runtime precedence (month > day >
--    cron) and clearing the lower-priority sources so the row is well-formed.
--
-- 2. Illegal interval values: negative or over-bound intervals (days > 3650,
--    months > 120) are coerced to bounds / cleared so they can compute a
--    next_run instead of being disabled.
--
-- 3. Backfill run_time for legacy interval rows: rows that run on an interval
--    but have empty run_time historically used next_run_at's time-of-day. Backfill
--    run_time from next_run_at's HH:MM so the executed time and the frontend
--    default no longer diverge; if next_run_at is NULL fall back to '09:00'.
--
-- STEP ORDER (PR#62 r4 Jerry-Xin Bug3): normalization (2a/2b) MUST run BEFORE
-- the mutual-exclusion precedence cleanup (1a/1b). The r3 ordering ran 1a first
-- with the predicate `interval_months > 0 AND interval_months <= 120`, so a row
-- with an OVER-BOUND month (e.g. interval_months=999 AND interval_days=1) failed
-- the `<= 120` guard and slipped past 1a; then 2b clamped 999 -> 120, leaving
-- BOTH interval_months=120 AND interval_days=1 set -- a double-source row the
-- scheduler (scheduler.go: mutual-exclusivity check) treats as invalid and
-- disables. By clamping/zeroing first (2a/2b) and THEN applying month>day>cron
-- precedence (1a/1b), the clamped 120 is in-range when 1a runs, so 1a clears the
-- competing interval_days/cron and the row ends up clean single-source
-- (interval_months=120, interval_days=0). The `<= 120` / `<= 3650` guards in
-- 1a/1b are now always satisfiable for any previously over-bound row.

-- 2a. Negative intervals -> 0 (treated as not-that-source). Run FIRST so the
--     subsequent precedence cleanup sees normalized, in-range values.
UPDATE summary_schedule SET interval_days = 0   WHERE interval_days < 0;
UPDATE summary_schedule SET interval_months = 0 WHERE interval_months < 0;

-- 2b. Over-bound intervals -> clamp to the max bound. Run BEFORE 1a/1b so an
--     over-bound month (e.g. 999) becomes 120 (in-range) and is then caught by
--     the month-wins precedence cleanup instead of slipping past it.
UPDATE summary_schedule SET interval_days = 3650  WHERE interval_days > 3650;
UPDATE summary_schedule SET interval_months = 120 WHERE interval_months > 120;

-- 1a. If both month and (day or cron) are set, month wins: clear the others.
--     After 2a/2b every month value is already in 0..120, so the `<= 120` guard
--     can no longer let a (clamped-from-over-bound) row escape this cleanup.
UPDATE summary_schedule
SET interval_days = 0, cron_expr = ''
WHERE interval_months > 0 AND interval_months <= 120
  AND (interval_days <> 0 OR cron_expr <> '');

-- 1b. If both day and cron are set (no valid month), day wins: clear cron.
UPDATE summary_schedule
SET cron_expr = ''
WHERE interval_days > 0 AND interval_days <= 3650
  AND (interval_months = 0 OR interval_months IS NULL)
  AND cron_expr <> '';

-- 3. Backfill run_time for interval rows missing it.
-- Use next_run_at's HH:MM when available (DATE_FORMAT), else 09:00.
UPDATE summary_schedule
SET run_time = DATE_FORMAT(next_run_at, '%H:%i')
WHERE (interval_days > 0 OR interval_months > 0)
  AND (run_time IS NULL OR run_time = '')
  AND next_run_at IS NOT NULL;

UPDATE summary_schedule
SET run_time = '09:00'
WHERE (interval_days > 0 OR interval_months > 0)
  AND (run_time IS NULL OR run_time = '')
  AND next_run_at IS NULL;

-- +migrate Down
-- Data cleansing is not losslessly reversible (we cannot reconstruct the exact
-- prior dirty state). Down is a no-op; the columns themselves are dropped by the
-- prior migration's Down. Pre-change snapshots are recorded in the deploy report.
SELECT 1;
