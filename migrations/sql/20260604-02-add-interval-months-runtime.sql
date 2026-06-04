-- +migrate Up
ALTER TABLE summary_schedule
    ADD COLUMN interval_months INT NOT NULL DEFAULT 0 COMMENT '间隔月数: 0=不按月, >0=每 N 个自然月(AddDate 推进)',
    ADD COLUMN run_time VARCHAR(5) NOT NULL DEFAULT '' COMMENT '间隔模式的运行时刻 HH:MM (UTC); 空=沿用基准时刻; cron 模式忽略';

-- +migrate Down
ALTER TABLE summary_schedule
    DROP COLUMN interval_months,
    DROP COLUMN run_time;
