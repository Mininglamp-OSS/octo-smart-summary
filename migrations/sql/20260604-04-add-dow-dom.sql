-- +migrate Up
-- Need 4: explicit weekday (week mode) / day-of-month (month mode) selection.
--
-- day_of_week aligns the WEEK mode (interval_days as a multiple of 7) to a
-- specific weekday: 1=Mon..7=Sun, 0=unconstrained (legacy behavior: natural
-- next_run_at progression).
--
-- day_of_month aligns the MONTH mode (interval_months > 0) to a specific day:
-- 1..31, clamped to the last day of shorter months at runtime (e.g. 31 -> Feb
-- 28/29); 0=unconstrained.
ALTER TABLE summary_schedule
    ADD COLUMN day_of_week TINYINT NOT NULL DEFAULT 0 COMMENT '周模式指定周几: 1=周一..7=周日, 0=不限',
    ADD COLUMN day_of_month TINYINT NOT NULL DEFAULT 0 COMMENT '月模式指定几号: 1..31(月末钳位), 0=不限';

-- +migrate Down
ALTER TABLE summary_schedule
    DROP COLUMN day_of_week,
    DROP COLUMN day_of_month;
