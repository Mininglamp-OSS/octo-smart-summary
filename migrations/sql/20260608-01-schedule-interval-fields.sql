-- +migrate Up
-- Scheduled summary: recurrence configuration for summary_schedule.
--
-- Adds the interval-based recurrence model (alongside the legacy cron_expr):
--   interval_days   : >0 => run every N days (e.g. 14 = bi-weekly); 0 = unused
--   interval_months : >0 => run every N calendar months (AddDate); 0 = unused
--   run_time        : HH:MM (Asia/Shanghai) time-of-day for interval modes;
--                     '' = inherit the anchor's time-of-day; ignored by cron
--   day_of_week     : week mode (interval_days multiple of 7) weekday anchor,
--                     1=Mon..7=Sun, 0=unconstrained
--   day_of_month    : month mode (interval_months>0) day anchor, 1..31
--                     (clamped to month end at runtime), 0=unconstrained
--   anchor_dom      : persisted original day-of-month so monthly recompute does
--                     not drift after a short month clamps the date
ALTER TABLE `summary_schedule`
    ADD COLUMN `interval_days`   INT     NOT NULL DEFAULT 0  COMMENT '间隔天数: 0=不用, >0=每 N 天(如14=每两周)' AFTER `cron_expr`,
    ADD COLUMN `interval_months` INT     NOT NULL DEFAULT 0  COMMENT '间隔月数: 0=不用, >0=每 N 个自然月' AFTER `interval_days`,
    ADD COLUMN `run_time`        VARCHAR(5) NOT NULL DEFAULT '' COMMENT '间隔模式运行时刻 HH:MM(Asia/Shanghai); 空=沿用基准时刻; cron 忽略' AFTER `interval_months`,
    ADD COLUMN `day_of_week`     TINYINT NOT NULL DEFAULT 0  COMMENT '周模式指定周几: 1=周一..7=周日, 0=不限' AFTER `run_time`,
    ADD COLUMN `day_of_month`    TINYINT NOT NULL DEFAULT 0  COMMENT '月模式指定几号: 1..31(月末钳位), 0=不限' AFTER `day_of_week`,
    ADD COLUMN `anchor_dom`      TINYINT NOT NULL DEFAULT 0  COMMENT '持久化月模式原始几号, 防止短月钳位后逐月漂移' AFTER `day_of_month`;

-- +migrate Down
ALTER TABLE `summary_schedule`
    DROP COLUMN `anchor_dom`,
    DROP COLUMN `day_of_month`,
    DROP COLUMN `day_of_week`,
    DROP COLUMN `run_time`,
    DROP COLUMN `interval_months`,
    DROP COLUMN `interval_days`;
