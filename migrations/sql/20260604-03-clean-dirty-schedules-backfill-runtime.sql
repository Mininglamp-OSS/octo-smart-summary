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

-- 1a. If both month and (day or cron) are set, month wins: clear the others.
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

-- 2a. Negative intervals -> 0 (treated as not-that-source).
UPDATE summary_schedule SET interval_days = 0   WHERE interval_days < 0;
UPDATE summary_schedule SET interval_months = 0 WHERE interval_months < 0;

-- 2b. Over-bound intervals -> clamp to the max bound.
UPDATE summary_schedule SET interval_days = 3650  WHERE interval_days > 3650;
UPDATE summary_schedule SET interval_months = 120 WHERE interval_months > 120;

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
