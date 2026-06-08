-- +migrate Up
-- PR#62 r11 P1-2: enforce one creator/participant row per (task_id,user_id) at
-- the DB layer, so concurrent worker bootstrap re-claims can never insert
-- duplicate creator participants (which would silently flip a scheduled task
-- into the multi-person path). 20260608-05 self-heals pre-existing duplicates
-- before this UNIQUE index is added so the migration cannot fail on dirty data.
-- Use plain MySQL CREATE/DROP INDEX ... ON ... (the conditional-existence
-- clause is unsupported by MySQL here); matches the existing 20260101-06
-- pattern. Re-run safety comes from sql-migrate's applied-version tracking.
CREATE UNIQUE INDEX `uk_summary_participant_task_user` ON `summary_participant` (`task_id`, `user_id`);

-- +migrate Down
DROP INDEX `uk_summary_participant_task_user` ON `summary_participant`;
